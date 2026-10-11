package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/platform"
	"github.com/ayeus/ayeusann/internal/usage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// maxInferenceBody bounds a prompt. Long-context requests are legitimate, so
// this is far above the control-plane limit. The coordinator and the agents
// accept the same size, so what is let in here fits all the way through.
const maxInferenceBody = platform.MaxInferenceBody

// maxAttempts is the first try plus retries on other replicas (SRS FR-33).
const maxAttempts = 3

// Trailers set by the coordinator's tunnel, and the header in which it says
// why it refused an attempt.
const (
	trailerPromptTokens     = "X-Usage-Prompt-Tokens"
	trailerCompletionTokens = "X-Usage-Completion-Tokens"
	trailerError            = "X-Upstream-Error"
	headerTunnelReason      = "X-Tunnel-Reason"
)

// Kinds of row in request_failures.
const (
	failedAttempt = "attempt" // one replica failed; another may have answered
	refused       = "refused" // the request was answered with an error
)

// principal is an authenticated caller: an API key or a console session.
type principal struct {
	OrgID string
	// RateKey identifies the caller for rate limiting.
	RateKey string
	// DeploymentScope is set for deployment-scoped API keys (SRS FR-5).
	DeploymentScope *string
}

// target is the deployment a request resolves to.
type target struct {
	ID, Name, ModelID, ModelName, State, Tier string
}

// Gateway serves the OpenAI-compatible inference API (SRS FR-30, IR-2).
type Gateway struct {
	db             *db.Client
	tm             *auth.TokenManager
	revocations    auth.RevocationStore
	limiter        *RateLimiter
	router         *router
	svcAuth        *auth.ServiceAuthenticator
	coordinatorURL string
	inferenceHost  string // e.g. "inference.example.com"; enables {dep-id}.inference.example.com
	client         *http.Client
	log            *slog.Logger
	// timeout is the limit on one request from start to finish.
	timeout time.Duration
}

// ─── OpenAI error format ──────────────────────────────────────

func writeOpenAIError(w http.ResponseWriter, status int, errType, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": errType, "code": code, "param": nil},
	})
}

// ─── Authentication ───────────────────────────────────────────

var errUnauthenticated = errors.New("unauthenticated")

func (g *Gateway) authenticate(r *http.Request) (*principal, error) {
	raw := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if raw == "" {
		if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
			raw = strings.TrimSpace(h[7:])
		}
	}
	if raw == "" {
		return nil, fmt.Errorf("%w: missing API key; send Authorization: Bearer sk_live_...", errUnauthenticated)
	}

	if strings.HasPrefix(raw, auth.APIKeyPrefix) {
		prefix, err := auth.ExtractPrefix(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: malformed API key", errUnauthenticated)
		}
		var id, orgID string
		var depID *string
		err = g.db.Pool.QueryRow(r.Context(), `
			SELECT id, org_id, deployment_id FROM api_keys
			WHERE prefix = $1 AND hash = $2 AND NOT revoked;
		`, prefix, auth.HashAPIKey(raw)).Scan(&id, &orgID, &depID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				platform.AuthFailuresTotal.WithLabelValues("inference-gateway", "bad_key").Inc()
				return nil, fmt.Errorf("%w: invalid or revoked API key", errUnauthenticated)
			}
			return nil, err
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _ = g.db.Pool.Exec(ctx, `UPDATE api_keys SET last_used_at = NOW() WHERE id = $1;`, id)
		}()
		return &principal{OrgID: orgID, RateKey: "key:" + id, DeploymentScope: depID}, nil
	}

	// A console session token. The console playground calls the same API as a
	// customer's code, so what works in the browser works with curl.
	claims, err := g.tm.VerifyAccessToken(raw)
	if err != nil {
		platform.AuthFailuresTotal.WithLabelValues("inference-gateway", "bad_token").Inc()
		return nil, fmt.Errorf("%w: invalid API key or session", errUnauthenticated)
	}
	if revoked, err := g.revocations.IsRevoked(r.Context(), claims.ID); err != nil || revoked {
		return nil, fmt.Errorf("%w: session has been revoked", errUnauthenticated)
	}
	if claims.OrgID == "" {
		return nil, fmt.Errorf("%w: session has no organisation", errUnauthenticated)
	}
	return &principal{OrgID: claims.OrgID, RateKey: "user:" + claims.UserID}, nil
}

// ─── Deployment resolution ────────────────────────────────────

// resolve finds the deployment a request addresses. In order:
//  1. the {deployment-id}.inference.<domain> host (SRS FR-22)
//  2. "model" as a deployment id or deployment name in the caller's org
//  3. "model" as a catalogue model name → the org's newest serving deployment
func (g *Gateway) resolve(ctx context.Context, r *http.Request, p *principal, model string) (*target, error) {
	const sel = `
		SELECT d.id, d.name, d.model_id, m.name, d.state, d.tier
		FROM deployments d JOIN models m ON m.id = d.model_id
		WHERE d.org_id = $1 AND d.deleted_at IS NULL AND d.desired_state <> 'stopped'`

	scan := func(row pgx.Row) (*target, error) {
		var t target
		if err := row.Scan(&t.ID, &t.Name, &t.ModelID, &t.ModelName, &t.State, &t.Tier); err != nil {
			return nil, err
		}
		return &t, nil
	}

	if g.inferenceHost != "" {
		host := strings.Split(r.Host, ":")[0]
		if sub, ok := strings.CutSuffix(host, "."+g.inferenceHost); ok {
			if _, err := uuid.Parse(sub); err == nil {
				return scan(g.db.Pool.QueryRow(ctx, sel+` AND d.id = $2;`, p.OrgID, sub))
			}
		}
	}

	if model == "" {
		return nil, pgx.ErrNoRows
	}
	if _, err := uuid.Parse(model); err == nil {
		if t, err := scan(g.db.Pool.QueryRow(ctx, sel+` AND d.id = $2;`, p.OrgID, model)); err == nil {
			return t, nil
		}
	}
	if t, err := scan(g.db.Pool.QueryRow(ctx, sel+` AND d.name = $2;`, p.OrgID, model)); err == nil {
		return t, nil
	}
	return scan(g.db.Pool.QueryRow(ctx, sel+` AND m.name = $2
		ORDER BY (d.state IN ('serving', 'degraded')) DESC, d.created_at DESC LIMIT 1;`, p.OrgID, model))
}

// ─── /v1/chat/completions, /v1/completions, /v1/embeddings ────

// requestID is the one id a request carries from the public gateway to the
// host. The gateway in front issues it; an id that is not a UUID is not one
// of ours and is replaced, since usage is recorded once per id.
func requestID(r *http.Request) string {
	if id, err := uuid.Parse(r.Header.Get("X-Request-ID")); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

// HandleInference proxies an OpenAI-compatible request to a replica.
func (g *Gateway) HandleInference(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	requestID := requestID(r)
	w.Header().Set("X-Request-ID", requestID)

	p, err := g.authenticate(r)
	if err != nil {
		if errors.Is(err, errUnauthenticated) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", err.Error())
			return
		}
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "internal_error", "authentication failed")
		return
	}

	if g.limiter != nil {
		if res, rlErr := g.limiter.Check(r.Context(), p.RateKey); rlErr == nil {
			g.limiter.WriteRateLimitHeaders(w, res)
			if !res.Allowed {
				writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded",
					"Rate limit exceeded; retry after the Retry-After interval")
				return
			}
		}
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInferenceBody))
	if err != nil {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large",
			fmt.Sprintf("request body is too large; the limit is %d MB", maxInferenceBody>>20))
		return
	}
	var envelope struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "request body must be JSON")
		return
	}

	tgt, err := g.resolve(r.Context(), r, p, envelope.Model)
	if err != nil {
		msg := fmt.Sprintf("No deployment named %q in your organisation. Use a deployment name from GET /v1/models.", envelope.Model)
		if envelope.Model == "" {
			msg = `"model" is required: pass your deployment name`
		}
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", msg)
		return
	}
	if p.DeploymentScope != nil && *p.DeploymentScope != tgt.ID {
		writeOpenAIError(w, http.StatusForbidden, "invalid_request_error", "key_scope",
			"This API key is scoped to a different deployment")
		return
	}
	if tgt.State != "serving" && tgt.State != "degraded" {
		g.recordFailure(r, failure{RequestID: requestID, OrgID: p.OrgID, Target: tgt, Kind: refused,
			Reason: "deployment_not_ready", Detail: "state: " + tgt.State, Status: http.StatusServiceUnavailable})
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", "deployment_not_ready",
			fmt.Sprintf("Deployment %q is not serving yet (state: %s).", tgt.Name, tgt.State))
		return
	}

	cands, err := g.router.candidates(r.Context(), tgt.ID)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "internal_error", "failed to discover replicas")
		return
	}

	// The whole request, retries included, has one limit.
	ctx, stop := context.WithTimeout(r.Context(), g.limit()+15*time.Second)
	defer stop()

	exclude := map[string]bool{}
	busy := false
	for attempt := 0; attempt < maxAttempts; attempt++ {
		rep, found, full := g.router.take(cands, exclude)
		if !found {
			busy = full
			break
		}
		platform.RouterReplicaSelections.WithLabelValues(rep.HostTier, "selected").Inc()

		resp, err := g.tunnel(ctx, requestID, rep.ID, r.URL.Path, body, envelope.Stream)
		if err != nil || resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout {
			g.router.release(rep.ID)
			exclude[rep.ID] = true
			if r.Context().Err() != nil {
				// The caller left; nothing failed.
				if resp != nil {
					resp.Body.Close()
				}
				return
			}
			f := failure{RequestID: requestID, OrgID: p.OrgID, Target: tgt, Replica: &rep, Kind: failedAttempt, Reason: "unreachable"}
			if err == nil {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
				resp.Body.Close()
				f.Detail, f.Status = strings.TrimSpace(string(b)), resp.StatusCode
				if reason := resp.Header.Get(headerTunnelReason); reason != "" {
					f.Reason = reason
				}
			} else {
				f.Detail = err.Error()
			}
			// A request too large for one host's old agent says nothing
			// about that host's health.
			if f.Reason != "agent_too_old" {
				g.router.markFailed(rep.ID)
			}
			g.recordFailure(r, f)
			g.log.Warn("replica attempt failed; retrying on another replica",
				"request_id", requestID, "replica_id", rep.ID, "attempt", attempt+1, "reason", f.Reason, "detail", f.Detail)
			if ctx.Err() != nil {
				// Out of time. Asking another replica now would only fail at
				// once and be held against a replica that did nothing wrong.
				g.recordFailure(r, failure{RequestID: requestID, OrgID: p.OrgID, Target: tgt, Kind: refused,
					Reason: "timeout", Status: http.StatusGatewayTimeout})
				writeOpenAIError(w, http.StatusGatewayTimeout, "server_error", "timeout",
					fmt.Sprintf("The request did not finish within %s.", g.limit()))
				return
			}
			continue
		}

		g.router.markHealthy(rep.ID)
		status, streamed := g.relay(w, resp, envelope.Stream)
		g.router.release(rep.ID)
		g.meter(r, requestID, p, tgt, rep, resp, status, start, streamed, estimatePromptTokens(body))
		if status == statusUpstreamLost && !envelope.Stream {
			// Part of a JSON body went out and the rest never will. Closing
			// the connection is the only way left to say it is not complete.
			panic(http.ErrAbortHandler)
		}
		return
	}

	platform.InferenceRequestsTotal.WithLabelValues(tgt.ModelName, tgt.Tier, "error").Inc()
	if busy {
		// Every replica is answering as many requests as it is allowed. That
		// is a queue, not a fault: client libraries wait and try again on 429.
		platform.RouterReplicaSelections.WithLabelValues(tgt.Tier, "at_capacity").Inc()
		g.recordFailure(r, failure{RequestID: requestID, OrgID: p.OrgID, Target: tgt, Kind: refused,
			Reason: "at_capacity", Status: http.StatusTooManyRequests})
		w.Header().Set("Retry-After", "1")
		writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", "at_capacity",
			"This deployment is answering as many requests as it can right now; retry shortly.")
		return
	}
	platform.RouterReplicaSelections.WithLabelValues(tgt.Tier, "no_capacity").Inc()
	g.recordFailure(r, failure{RequestID: requestID, OrgID: p.OrgID, Target: tgt, Kind: refused,
		Reason: "no_healthy_replica", Status: http.StatusServiceUnavailable})
	writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", "no_healthy_replica",
		"No healthy replica is available for this deployment right now; retry shortly.")
}

// limit is the time one request may take.
func (g *Gateway) limit() time.Duration {
	if g.timeout > 0 {
		return g.timeout
	}
	return 10 * time.Minute
}

// failure is a row for request_failures.
type failure struct {
	RequestID, OrgID string
	Target           *target
	Replica          *replica // nil when no replica was involved
	Kind, Reason     string
	Detail           string
	Status           int
}

// recordFailure writes down an attempt that failed or a request that was
// turned away. It runs detached from the caller's context: a caller who has
// already hung up is exactly when the record matters.
func (g *Gateway) recordFailure(r *http.Request, f failure) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	var replicaID, hostID *string
	if f.Replica != nil {
		replicaID, hostID = &f.Replica.ID, &f.Replica.HostID
	}
	if len(f.Detail) > 500 {
		f.Detail = f.Detail[:500]
	}
	if _, err := g.db.Pool.Exec(ctx, `
		INSERT INTO request_failures (request_id, org_id, deployment_id, replica_id, host_id, model_id, kind, reason, detail, status_code)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);
	`, f.RequestID, f.OrgID, f.Target.ID, replicaID, hostID, f.Target.ModelID, f.Kind, f.Reason, f.Detail, f.Status); err != nil {
		g.log.Error("failed to record a request failure", "request_id", f.RequestID, "reason", f.Reason, "err", err)
	}
}

// tunnel opens the coordinator's inference stream for one replica.
func (g *Gateway) tunnel(ctx context.Context, requestID, replicaID, path string, body []byte, stream bool) (*http.Response, error) {
	payload, err := json.Marshal(map[string]any{
		"request_id":      requestID,
		"replica_id":      replicaID,
		"path":            path,
		"body":            json.RawMessage(body),
		"stream":          stream,
		"timeout_seconds": int(g.limit() / time.Second),
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.coordinatorURL+"/internal/v1/infer", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	g.svcAuth.SignRequest(req)
	return g.client.Do(req)
}

// Statuses relay reports beyond those of HTTP.
const (
	statusClientGone   = 499 // the caller hung up
	statusUpstreamLost = 598 // the answer stopped arriving part-way
)

// relay copies the replica's response to the client, flushing as bytes arrive
// so server-sent events reach the client token by token.
//
// It also counts the content deltas of a stream as they pass. Runtimes emit one
// delta per token, so if the client disconnects before the final usage report
// arrives the request is still recorded with what was generated.
//
// A stream whose source disappears part-way is ended with an error event, so
// the caller is not left holding half an answer that looks finished.
func (g *Gateway) relay(w http.ResponseWriter, resp *http.Response, stream bool) (int, int) {
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	var counter deltaCounter
	buf := make([]byte, 16<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			counter.write(buf[:n])
			if _, werr := w.Write(buf[:n]); werr != nil {
				return statusClientGone, counter.n
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return resp.StatusCode, counter.n
			}
			if resp.Request != nil && resp.Request.Context().Err() != nil && !errors.Is(resp.Request.Context().Err(), context.DeadlineExceeded) {
				return statusClientGone, counter.n
			}
			if stream && resp.StatusCode < 400 {
				b, _ := json.Marshal(map[string]any{"error": map[string]any{
					"message": "The answer was interrupted: the connection to the host was lost",
					"type":    "server_error", "code": "stream_interrupted", "param": nil,
				}})
				_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
			return statusUpstreamLost, counter.n
		}
	}
}

// deltaCounter counts non-empty content deltas in an SSE byte stream.
type deltaCounter struct {
	line []byte
	n    int
}

func (c *deltaCounter) write(p []byte) {
	for _, b := range p {
		if b != '\n' {
			if len(c.line) < 1<<16 {
				c.line = append(c.line, b)
			}
			continue
		}
		if data, ok := bytes.CutPrefix(c.line, []byte("data:")); ok {
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
					Text string `json:"text"`
				} `json:"choices"`
			}
			if json.Unmarshal(bytes.TrimSpace(data), &chunk) == nil && len(chunk.Choices) > 0 &&
				(chunk.Choices[0].Delta.Content != "" || chunk.Choices[0].Text != "") {
				c.n++
			}
		}
		c.line = c.line[:0]
	}
}

// estimatePromptTokens approximates prompt size (~4 characters per token) for
// the one case with no exact count: a stream the client abandoned.
func estimatePromptTokens(body []byte) int {
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Prompt json.RawMessage `json:"prompt"`
	}
	if json.Unmarshal(body, &req) != nil {
		return 0
	}
	chars := len(req.Prompt)
	for _, m := range req.Messages {
		chars += len(m.Content)
	}
	return (chars + 3) / 4
}

// meter records the request. It runs detached from the client's context so a
// client hanging up after the last token does not skip the record.
func (g *Gateway) meter(r *http.Request, requestID string, p *principal, tgt *target, rep replica, resp *http.Response, status int, start time.Time, streamed, promptEstimate int) {
	prompt, _ := strconv.Atoi(resp.Trailer.Get(trailerPromptTokens))
	completion, _ := strconv.Atoi(resp.Trailer.Get(trailerCompletionTokens))
	upstreamErr := resp.Trailer.Get(trailerError)

	outcome := usage.StatusSuccess
	switch {
	case status == statusClientGone:
		outcome = usage.StatusCancelled
	case status >= 400 || upstreamErr != "":
		outcome = usage.StatusError
	}
	// A cancelled stream never delivers its usage trailer. Record the tokens
	// that were actually streamed rather than nothing.
	if outcome == usage.StatusCancelled && completion == 0 && streamed > 0 {
		completion = streamed
		if prompt == 0 {
			prompt = promptEstimate
		}
	}
	elapsed := time.Since(start)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()
	if _, err := usage.Record(ctx, g.db, usage.Event{
		RequestID:    requestID,
		OrgID:        p.OrgID,
		DeploymentID: tgt.ID,
		ReplicaID:    rep.ID,
		HostID:       rep.HostID,
		ModelID:      tgt.ModelID,
		Tier:         rep.HostTier,
		InputTokens:  prompt,
		OutputTokens: completion,
		GPUSeconds:   elapsed.Seconds(),
		DurationMs:   int(elapsed.Milliseconds()),
		Status:       outcome,
	}); err != nil {
		platform.UsageRecordFailures.Inc()
		g.log.Error("failed to record usage", "request_id", requestID, "err", err)
		return
	}

	platform.InferenceRequestsTotal.WithLabelValues(tgt.ModelName, rep.HostTier, map[bool]string{true: "success", false: "error"}[outcome == usage.StatusSuccess]).Inc()
	platform.InferenceTokensTotal.WithLabelValues(tgt.ModelName, "input").Add(float64(prompt))
	platform.InferenceTokensTotal.WithLabelValues(tgt.ModelName, "output").Add(float64(completion))
	platform.InferenceLatency.WithLabelValues(tgt.ModelName, rep.HostTier).Observe(elapsed.Seconds())
	g.log.Info("inference served", "request_id", requestID, "deployment_id", tgt.ID, "replica_id", rep.ID,
		"status", outcome, "prompt_tokens", prompt, "completion_tokens", completion, "ms", elapsed.Milliseconds())
}

// ─── /v1/models ───────────────────────────────────────────────

// HandleListModels lists the caller's callable deployments in OpenAI format.
// The "id" is the deployment name, which is what "model" should be set to.
func (g *Gateway) HandleListModels(w http.ResponseWriter, r *http.Request) {
	p, err := g.authenticate(r)
	if err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", err.Error())
		return
	}
	rows, err := g.db.Pool.Query(r.Context(), `
		SELECT d.id, d.name, m.name, d.state, EXTRACT(EPOCH FROM d.created_at)::BIGINT
		FROM deployments d JOIN models m ON m.id = d.model_id
		WHERE d.org_id = $1 AND d.deleted_at IS NULL AND d.desired_state = 'running'
		ORDER BY d.created_at DESC;
	`, p.OrgID)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "internal_error", "failed to list models")
		return
	}
	defer rows.Close()

	type entry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
		Root    string `json:"root"`
		State   string `json:"state"`
	}
	data := []entry{}
	for rows.Next() {
		var id, name, root, state string
		var created int64
		if err := rows.Scan(&id, &name, &root, &state, &created); err != nil {
			continue
		}
		if p.DeploymentScope != nil && *p.DeploymentScope != id {
			continue
		}
		data = append(data, entry{ID: name, Object: "model", Created: created, OwnedBy: "organization", Root: root, State: state})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}
