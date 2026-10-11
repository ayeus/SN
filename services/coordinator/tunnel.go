package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	agentv1 "github.com/ayeus/ayeusann/gen/go/agent/v1"
	"github.com/ayeus/ayeusann/internal/httpx"
	"github.com/ayeus/ayeusann/internal/platform"
	"github.com/jackc/pgx/v5"
)

// Trailers carrying the runtime's exact token counts back to the inference
// gateway once the body has been fully streamed.
const (
	TrailerPromptTokens     = "X-Usage-Prompt-Tokens"
	TrailerCompletionTokens = "X-Usage-Completion-Tokens"
	TrailerError            = "X-Upstream-Error"
)

// HeaderReason says, on a refusal, whose fault it was. The inference gateway
// records it with the failed attempt, and only the reasons that are the
// host's doing count against the host.
const HeaderReason = "X-Tunnel-Reason"

const (
	reasonNotServing   = "not_serving"       // the replica is not in a state to answer
	reasonDisconnected = "host_disconnected" // the host has no session, or lost it
	reasonHostError    = "host_error"        // the host or its runtime answered with an error
	reasonHostTimeout  = "host_timeout"      // the host went quiet or ran out of time
	reasonAgentTooOld  = "agent_too_old"     // the request needs a newer agent
)

const (
	// keepaliveEvery is how often an agent is asked to say it is still working.
	keepaliveEvery = 10 * time.Second
	// silenceLimit is how long such an agent may say nothing at all before the
	// request is given up on: several missed keep-alives, not one late one.
	silenceLimit = 45 * time.Second
	// legacyMaxBody is the largest request an agent without large-message
	// support can take: its connection breaks on a 4 MB message.
	legacyMaxBody = 4<<20 - 64<<10
)

// inferRequest is the internal tunnel request from the inference gateway.
type inferRequest struct {
	RequestID      string          `json:"request_id"`
	ReplicaID      string          `json:"replica_id"`
	Path           string          `json:"path"`
	Body           json.RawMessage `json:"body"`
	Stream         bool            `json:"stream"`
	TimeoutSeconds int             `json:"timeout_seconds"`
}

var allowedPaths = map[string]bool{
	"/v1/chat/completions": true,
	"/v1/completions":      true,
	"/v1/embeddings":       true,
}

// refuse answers a request that has not been committed to. The gateway may
// try another replica.
func refuse(w http.ResponseWriter, status int, reason, detail string) {
	w.Header().Set(HeaderReason, reason)
	httpx.WriteProblem(w, status, detail)
}

// streamError is the event that ends a stream which could not be finished.
// Without it a stream cut short looks exactly like one that completed, and
// the caller keeps half an answer believing it is whole. OpenAI's own API
// reports a failure inside a stream the same way, so client libraries raise
// it as an error.
func streamError(message string) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": message, "type": "server_error", "code": "stream_interrupted", "param": nil,
	}})
	return []byte("data: " + string(b) + "\n\n")
}

// handleInfer tunnels one inference request to the agent holding the replica
// and streams the answer back. This is the data plane of ADR-011: hosts never
// accept inbound connections, so requests ride the agent's outbound stream.
//
// A request that fails before anything has been sent to the caller is refused
// with 502, 503 or 504, and the gateway tries another replica. An answer that
// is not streamed is held here until it is complete, so it is either whole or
// refused. A stream is committed at its first byte; if it then breaks, it ends
// with an error event.
func (s *AgentServer) handleInfer(w http.ResponseWriter, r *http.Request) {
	var req inferRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, platform.MaxAgentMessage)).Decode(&req); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid tunnel request")
		return
	}
	if req.RequestID == "" || req.ReplicaID == "" || !allowedPaths[req.Path] {
		httpx.WriteProblem(w, http.StatusBadRequest, "request_id, replica_id and a supported path are required")
		return
	}

	ctx := r.Context()
	var hostID, state string
	err := s.db.Pool.QueryRow(ctx, `SELECT host_id, state FROM replicas WHERE id = $1;`, req.ReplicaID).Scan(&hostID, &state)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteProblem(w, http.StatusNotFound, "replica not found")
			return
		}
		httpx.WriteProblem(w, http.StatusInternalServerError, "replica lookup failed")
		return
	}
	if state != "serving" {
		refuse(w, http.StatusServiceUnavailable, reasonNotServing, "replica is not serving (state: "+state+")")
		return
	}
	sess := s.sessions.get(hostID)
	if sess == nil {
		refuse(w, http.StatusServiceUnavailable, reasonDisconnected, "replica's host is not connected")
		return
	}
	if len(req.Body) > legacyMaxBody && !sess.has(capLargeMessages) {
		// Sending it would break the host's connection and with it every
		// other request the host is answering.
		refuse(w, http.StatusServiceUnavailable, reasonAgentTooOld, "the host's agent is too old to take a request this large")
		return
	}

	ans, err := sess.open(req.RequestID)
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, reasonDisconnected, err.Error())
		return
	}
	defer sess.release(req.RequestID)

	// One limit for the whole request, whoever set it and however the answer
	// is delivered.
	limit := s.inferenceTimeout
	if limit <= 0 {
		limit = 10 * time.Minute
	}
	if t := time.Duration(req.TimeoutSeconds) * time.Second; t > 0 && t < limit {
		limit = t
	}
	every, quiet := s.keepaliveEvery, s.silenceLimit
	if every <= 0 {
		every, quiet = keepaliveEvery, silenceLimit
	}
	keepalive := sess.has(capKeepalive)
	msg := &agentv1.InferenceRequest{
		RequestId:      req.RequestID,
		ReplicaId:      req.ReplicaID,
		Path:           req.Path,
		Body:           req.Body,
		Stream:         req.Stream,
		TimeoutSeconds: int32(limit / time.Second),
	}
	if keepalive {
		msg.KeepaliveSeconds = int32(max(every/time.Second, 1))
	}
	if err := sess.send(&agentv1.CoordinatorMessage{Payload: &agentv1.CoordinatorMessage_InferenceRequest{InferenceRequest: msg}}); err != nil {
		refuse(w, http.StatusServiceUnavailable, reasonDisconnected, "failed to reach host")
		return
	}

	cancel := func() {
		_ = sess.send(&agentv1.CoordinatorMessage{Payload: &agentv1.CoordinatorMessage_InferenceCancel{
			InferenceCancel: &agentv1.InferenceCancel{RequestId: req.RequestID},
		}})
	}

	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	// An agent that sends keep-alives is never silent for long while it is
	// working, so silence means it is gone. One that does not send them can
	// only be given the whole limit.
	var silence *time.Timer
	var silent <-chan time.Time
	if keepalive {
		silence = time.NewTimer(quiet)
		defer silence.Stop()
		silent = silence.C
	}

	// next returns the next chunk that carries something. When there will be
	// none, it returns nil and why; the host has then been told to stop.
	// gone means the caller left and there is nobody to answer.
	next := func() (c *agentv1.InferenceChunk, reason, detail string, gone bool) {
		for {
			if c := ans.pop(); c != nil {
				if silence != nil {
					silence.Reset(quiet)
				}
				if c.GetKeepalive() && !c.GetDone() && len(c.GetData()) == 0 {
					continue
				}
				return c, "", "", false
			}
			select {
			case <-ans.wake:
			case <-ctx.Done():
				cancel()
				return nil, "", "", true
			case <-silent:
				cancel()
				return nil, reasonHostTimeout, "the host stopped responding", false
			case <-deadline.C:
				cancel()
				return nil, reasonHostTimeout, "the request took longer than " + limit.String(), false
			}
		}
	}
	// failed names the reason a final chunk reports failure for.
	failed := func(c *agentv1.InferenceChunk) string {
		if c.GetError() == errSessionClosed.Error() {
			return reasonDisconnected
		}
		return reasonHostError
	}
	usage := func(c *agentv1.InferenceChunk) {
		w.Header().Set(TrailerPromptTokens, strconv.Itoa(int(c.GetPromptTokens())))
		w.Header().Set(TrailerCompletionTokens, strconv.Itoa(int(c.GetCompletionTokens())))
	}
	h := w.Header()
	h.Set("Trailer", TrailerPromptTokens+", "+TrailerCompletionTokens+", "+TrailerError)

	if !req.Stream {
		// Collected whole before a status is sent: an answer that breaks half
		// way is refused, and the gateway can ask another replica for it.
		var body []byte
		status := 0
		for {
			c, reason, detail, gone := next()
			if gone {
				return
			}
			if c == nil {
				refuse(w, http.StatusGatewayTimeout, reason, detail)
				return
			}
			if status == 0 {
				status = int(c.GetStatusCode())
			}
			if c.GetError() != "" {
				code := int(c.GetStatusCode())
				if code < 400 {
					code = http.StatusBadGateway
				}
				refuse(w, code, failed(c), "replica error: "+c.GetError())
				return
			}
			if len(body)+len(c.GetData()) > maxBufferedAnswer {
				cancel()
				refuse(w, http.StatusBadGateway, reasonHostError, "replica error: the answer is too large")
				return
			}
			body = append(body, c.GetData()...)
			if c.GetDone() {
				if status == 0 {
					status = http.StatusOK
				}
				h.Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write(body)
				usage(c)
				return
			}
		}
	}

	first, reason, detail, gone := next()
	if gone {
		return
	}
	if first == nil {
		refuse(w, http.StatusGatewayTimeout, reason, detail)
		return
	}
	if first.GetError() != "" && len(first.GetData()) == 0 {
		code := int(first.GetStatusCode())
		if code < 400 {
			code = http.StatusBadGateway
		}
		refuse(w, code, failed(first), "replica error: "+first.GetError())
		return
	}

	status := int(first.GetStatusCode())
	if status == 0 {
		status = http.StatusOK
	}
	if status >= 400 {
		// The runtime turned the request down; its explanation is JSON.
		h.Set("Content-Type", "application/json")
	} else {
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
	}
	w.WriteHeader(status)
	flusher, _ := w.(http.Flusher)
	// interrupted ends a stream that cannot be finished.
	interrupted := func(why string) {
		if status < 400 {
			_, _ = w.Write(streamError("The answer was interrupted: " + why))
		}
		h.Set(TrailerError, why)
	}

	chunk := first
	for {
		if len(chunk.GetData()) > 0 {
			if _, err := w.Write(chunk.GetData()); err != nil {
				cancel()
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if chunk.GetDone() {
			usage(chunk)
			if chunk.GetError() != "" {
				interrupted(chunk.GetError())
			}
			return
		}
		var gone bool
		if chunk, _, detail, gone = next(); gone {
			return
		}
		if chunk == nil {
			interrupted(detail)
			return
		}
	}
}

// handleConnected lists hosts with a live session. The inference gateway uses
// it to skip replicas whose host dropped since the last database update.
func (s *AgentServer) handleConnected(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"host_ids": s.sessions.connected()})
}
