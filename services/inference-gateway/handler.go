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
	"github.com/ayeus/ayeusann/internal/billing"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/platform"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// maxInferenceBody bounds a prompt. Long-context requests are legitimate, so
// this is far above the control-plane limit.
const maxInferenceBody = 8 << 20

// maxAttempts is the first try plus retries on other replicas (SRS FR-33).
const maxAttempts = 3

// Trailers set by the coordinator's tunnel.
const (
	trailerPromptTokens     = "X-Usage-Prompt-Tokens"
	trailerCompletionTokens = "X-Usage-Completion-Tokens"
	trailerError            = "X-Upstream-Error"
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
	ledger         *db.LedgerService
	tm             *auth.TokenManager
	revocations    auth.RevocationStore
	limiter        *RateLimiter
	router         *router
	svcAuth        *auth.ServiceAuthenticator
	coordinatorURL string
	inferenceHost  string // e.g. "inference.example.com"; enables {dep-id}.inference.example.com
	client         *http.Client
	log            *slog.Logger
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

// HandleInference proxies an OpenAI-compatible request to a replica.
func (g *Gateway) HandleInference(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	requestID := uuid.NewString()
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
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "request body is too large")
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
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", "deployment_not_ready",
			fmt.Sprintf("Deployment %q is not serving yet (state: %s).", tgt.Name, tgt.State))
		return
	}

	// Admission control: a request starts only if the wallet can pay for it.
	ok, _, err := g.ledger.HasSpendableBalance(r.Context(), p.OrgID)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "internal_error", "failed to check wallet balance")
		return
	}
	if !ok {
		platform.InferenceRequestsTotal.WithLabelValues(tgt.ModelName, tgt.Tier, "rejected").Inc()
		writeOpenAIError(w, http.StatusPaymentRequired, "insufficient_quota", "insufficient_quota",
			"Your wallet balance is exhausted. Top up in the console to continue.")
		return
	}

	cands, err := g.router.candidates(r.Context(), tgt.ID)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "internal_error", "failed to discover replicas")
		return
	}

	exclude := map[string]bool{}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		rep, found := g.router.pick(cands, exclude)
		if !found {
			break
		}
		platform.RouterReplicaSelections.WithLabelValues(rep.HostTier, "selected").Inc()

		g.router.acquire(rep.ID)
		resp, err := g.tunnel(r.Context(), requestID, rep.ID, r.URL.Path, body, envelope.Stream)
		if err != nil || resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout {
			g.router.release(rep.ID)
			g.router.markFailed(rep.ID)
			exclude[rep.ID] = true
			reason := "tunnel error"
			if err == nil {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
				resp.Body.Close()
				reason = strings.TrimSpace(string(b))
			} else {
				reason = err.Error()
			}
			g.log.Warn("replica attempt failed; retrying on another replica",
				"request_id", requestID, "replica_id", rep.ID, "attempt", attempt+1, "reason", reason)
			if r.Context().Err() != nil {
				return
			}
			continue
		}

		g.router.markHealthy(rep.ID)
		status, streamed := g.relay(w, resp)
		g.router.release(rep.ID)
		g.meter(r, requestID, p, tgt, rep, resp, status, start, streamed, estimatePromptTokens(body))
		return
	}

	platform.RouterReplicaSelections.WithLabelValues(tgt.Tier, "no_capacity").Inc()
	platform.InferenceRequestsTotal.WithLabelValues(tgt.ModelName, tgt.Tier, "error").Inc()
	writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", "no_healthy_replica",
		"No healthy replica is available for this deployment right now; retry shortly.")
}

// tunnel opens the coordinator's inference stream for one replica.
func (g *Gateway) tunnel(ctx context.Context, requestID, replicaID, path string, body []byte, stream bool) (*http.Response, error) {
	payload, err := json.Marshal(map[string]any{
		"request_id": requestID,
		"replica_id": replicaID,
		"path":       path,
		"body":       json.RawMessage(body),
		"stream":     stream,
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

// relay copies the replica's response to the client, flushing as bytes arrive
// so server-sent events reach the client token by token.
//
// It also counts the content deltas of a stream as they pass. Runtimes emit one
// delta per token, so if the client disconnects before the final usage report
// arrives the request can still be billed for what was generated.
func (g *Gateway) relay(w http.ResponseWriter, resp *http.Response) (int, int) {
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
				return 499, counter.n // client went away
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return resp.StatusCode, counter.n
			}
			return 499, counter.n
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
// client hanging up after the last token does not skip billing.
func (g *Gateway) meter(r *http.Request, requestID string, p *principal, tgt *target, rep replica, resp *http.Response, status int, start time.Time, streamed, promptEstimate int) {
	prompt, _ := strconv.Atoi(resp.Trailer.Get(trailerPromptTokens))
	completion, _ := strconv.Atoi(resp.Trailer.Get(trailerCompletionTokens))
	upstreamErr := resp.Trailer.Get(trailerError)

	outcome := billing.StatusSuccess
	switch {
	case status == 499:
		outcome = billing.StatusCancelled
	case status >= 400 || upstreamErr != "":
		outcome = billing.StatusError
	}
	// A cancelled stream never delivers its usage trailer. Bill the tokens that
	// were actually streamed rather than nothing.
	if outcome == billing.StatusCancelled && completion == 0 && streamed > 0 {
		completion = streamed
		if prompt == 0 {
			prompt = promptEstimate
		}
	}
	elapsed := time.Since(start)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()
	res, err := billing.RecordUsage(ctx, g.db, billing.UsageInput{
		RequestID:    requestID,
		OrgID:        p.OrgID,
		DeploymentID: tgt.ID,
		ReplicaID:    rep.ID,
		HostID:       rep.HostID,
		ModelID:      tgt.ModelID,
		Tier:         rep.HostTier,
		GPUModel:     rep.GPUModel,
		InputTokens:  prompt,
		OutputTokens: completion,
		GPUSeconds:   elapsed.Seconds(),
		DurationMs:   int(elapsed.Milliseconds()),
		Status:       outcome,
	})
	if err != nil {
		platform.BillingDebitFailures.Inc()
		g.log.Error("failed to record usage", "request_id", requestID, "err", err)
		return
	}

	platform.InferenceRequestsTotal.WithLabelValues(tgt.ModelName, rep.HostTier, map[bool]string{true: "success", false: "error"}[outcome == billing.StatusSuccess]).Inc()
	platform.InferenceTokensTotal.WithLabelValues(tgt.ModelName, "input").Add(float64(prompt))
	platform.InferenceTokensTotal.WithLabelValues(tgt.ModelName, "output").Add(float64(completion))
	platform.InferenceLatency.WithLabelValues(tgt.ModelName, rep.HostTier).Observe(elapsed.Seconds())
	g.log.Info("inference served", "request_id", requestID, "deployment_id", tgt.ID, "replica_id", rep.ID,
		"status", outcome, "prompt_tokens", prompt, "completion_tokens", completion,
		"charge", res.Charge.String(), "currency", res.Charge.Currency(), "ms", elapsed.Milliseconds())
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
