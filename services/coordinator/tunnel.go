package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	agentv1 "github.com/ayeus/ayeusann/gen/go/agent/v1"
	"github.com/ayeus/ayeusann/internal/httpx"
	"github.com/jackc/pgx/v5"
)

// Trailers carrying the runtime's exact token counts back to the inference
// gateway once the body has been fully streamed.
const (
	TrailerPromptTokens     = "X-Usage-Prompt-Tokens"
	TrailerCompletionTokens = "X-Usage-Completion-Tokens"
	TrailerError            = "X-Upstream-Error"
)

// firstByteTimeout bounds how long the gateway waits for a replica to start
// answering. A loaded model starts in well under a second; this only fires on
// a wedged runtime.
const firstByteTimeout = 60 * time.Second

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

// handleInfer tunnels one inference request to the agent holding the replica
// and streams the answer back. This is the data plane of ADR-011: hosts never
// accept inbound connections, so requests ride the agent's outbound stream.
//
// Errors before the first byte are returned as 502/503 so the gateway can retry
// on another replica. Once bytes have been sent the stream is committed.
func (s *AgentServer) handleInfer(w http.ResponseWriter, r *http.Request) {
	var req inferRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&req); err != nil {
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
		httpx.WriteProblem(w, http.StatusServiceUnavailable, "replica is not serving (state: "+state+")")
		return
	}
	sess := s.sessions.get(hostID)
	if sess == nil {
		httpx.WriteProblem(w, http.StatusServiceUnavailable, "replica's host is not connected")
		return
	}

	ch, err := sess.open(req.RequestID)
	if err != nil {
		httpx.WriteProblem(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer sess.release(req.RequestID)

	timeout := req.TimeoutSeconds
	if timeout <= 0 || timeout > 600 {
		timeout = 300
	}
	if err := sess.send(&agentv1.CoordinatorMessage{Payload: &agentv1.CoordinatorMessage_InferenceRequest{
		InferenceRequest: &agentv1.InferenceRequest{
			RequestId:      req.RequestID,
			ReplicaId:      req.ReplicaID,
			Path:           req.Path,
			Body:           req.Body,
			Stream:         req.Stream,
			TimeoutSeconds: int32(timeout),
		},
	}}); err != nil {
		httpx.WriteProblem(w, http.StatusServiceUnavailable, "failed to reach host")
		return
	}

	cancel := func() {
		_ = sess.send(&agentv1.CoordinatorMessage{Payload: &agentv1.CoordinatorMessage_InferenceCancel{
			InferenceCancel: &agentv1.InferenceCancel{RequestId: req.RequestID},
		}})
	}

	// Wait for the first chunk before committing a status code.
	var first *agentv1.InferenceChunk
	select {
	case first = <-ch:
	case <-ctx.Done():
		cancel()
		return
	case <-time.After(firstByteTimeout):
		cancel()
		httpx.WriteProblem(w, http.StatusGatewayTimeout, "replica did not start responding within 60 s")
		return
	}
	if first.GetError() != "" && len(first.GetData()) == 0 {
		code := int(first.GetStatusCode())
		if code < 400 {
			code = http.StatusBadGateway
		}
		httpx.WriteProblem(w, code, "replica error: "+first.GetError())
		return
	}

	h := w.Header()
	h.Set("Trailer", TrailerPromptTokens+", "+TrailerCompletionTokens+", "+TrailerError)
	if req.Stream {
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
	} else {
		h.Set("Content-Type", "application/json")
	}
	status := int(first.GetStatusCode())
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	flusher, _ := w.(http.Flusher)

	deadline := time.NewTimer(time.Duration(timeout) * time.Second)
	defer deadline.Stop()

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
			h.Set(TrailerPromptTokens, strconv.Itoa(int(chunk.GetPromptTokens())))
			h.Set(TrailerCompletionTokens, strconv.Itoa(int(chunk.GetCompletionTokens())))
			if chunk.GetError() != "" {
				h.Set(TrailerError, chunk.GetError())
			}
			return
		}
		select {
		case chunk = <-ch:
		case <-ctx.Done():
			cancel()
			return
		case <-deadline.C:
			cancel()
			h.Set(TrailerError, "request exceeded its timeout")
			return
		}
	}
}

// handleConnected lists hosts with a live session. The inference gateway uses
// it to skip replicas whose host dropped since the last database update.
func (s *AgentServer) handleConnected(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"host_ids": s.sessions.connected()})
}
