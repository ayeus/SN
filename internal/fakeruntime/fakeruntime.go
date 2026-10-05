// Package fakeruntime is a stand-in for Ollama, used to test the platform end
// to end on machines with no GPU and no model runtime: CI, and laptops that
// should not download gigabytes to run a smoke test.
//
// It implements exactly the endpoints the host agent calls (agent/src/runtime.rs
// and agent/src/inference.rs) and answers with the shapes Ollama uses, so the
// agent, coordinator and gateways run their real code paths. It generates no
// text worth reading: replies are a fixed sentence, one word per token.
//
// Faults can be injected through Config, so tests can make a download fail, a
// model refuse to load, or an answer arrive slowly.
package fakeruntime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Config controls how the runtime behaves. The zero value is a fast, healthy
// runtime with nothing downloaded.
type Config struct {
	// FirstByteDelayMs delays the start of every completion.
	FirstByteDelayMs int `json:"first_byte_delay_ms"`
	// TokenDelayMs is the pause between streamed tokens.
	TokenDelayMs int `json:"token_delay_ms"`
	// Tokens is how many tokens a completion produces when the request does
	// not ask for fewer (default 8).
	Tokens int `json:"tokens"`
	// PullSteps and PullStepMs shape a download: that many progress lines,
	// that far apart (defaults 3 and 20 ms).
	PullSteps  int `json:"pull_steps"`
	PullStepMs int `json:"pull_step_ms"`
	// FailPull and FailLoad list models whose download or load fails.
	FailPull []string `json:"fail_pull"`
	FailLoad []string `json:"fail_load"`
	// Hang makes completions accept the request and never answer.
	Hang bool `json:"hang"`
	// Status, when not zero, is returned by every completion instead of an answer.
	Status int `json:"status"`
}

// Runtime is one fake model runtime.
type Runtime struct {
	mu     sync.Mutex
	cfg    Config
	pulled map[string]bool
	loaded map[string]bool
}

// New returns a runtime with the given models already downloaded.
func New(cfg Config, cached ...string) *Runtime {
	r := &Runtime{cfg: cfg, pulled: map[string]bool{}, loaded: map[string]bool{}}
	for _, m := range cached {
		r.pulled[m] = true
	}
	return r
}

// SetConfig replaces the fault configuration.
func (r *Runtime) SetConfig(cfg Config) {
	r.mu.Lock()
	r.cfg = cfg
	r.mu.Unlock()
}

// Loaded reports the models currently held in memory, sorted.
func (r *Runtime) Loaded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return sortedKeys(r.loaded)
}

func (r *Runtime) config() Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Handler serves the runtime's HTTP API.
func (r *Runtime) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": "0.0.0-fake"})
	})
	mux.HandleFunc("GET /api/tags", r.handleTags)
	mux.HandleFunc("GET /api/ps", r.handlePS)
	mux.HandleFunc("POST /api/pull", r.handlePull)
	mux.HandleFunc("POST /api/generate", r.handleGenerate)
	mux.HandleFunc("DELETE /api/delete", r.handleDelete)
	mux.HandleFunc("GET /v1/models", r.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, req *http.Request) { r.handleCompletion(w, req, true) })
	mux.HandleFunc("POST /v1/completions", func(w http.ResponseWriter, req *http.Request) { r.handleCompletion(w, req, false) })
	mux.HandleFunc("POST /v1/embeddings", r.handleEmbeddings)
	mux.HandleFunc("GET /_fake/config", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, r.config()) })
	mux.HandleFunc("POST /_fake/config", func(w http.ResponseWriter, req *http.Request) {
		var cfg Config
		if err := json.NewDecoder(req.Body).Decode(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid config: " + err.Error()})
			return
		}
		r.SetConfig(cfg)
		writeJSON(w, http.StatusOK, cfg)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ─── Model management (Ollama's native API) ───────────────────

func (r *Runtime) handleTags(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	names := sortedKeys(r.pulled)
	r.mu.Unlock()
	models := []map[string]any{}
	for _, n := range names {
		models = append(models, map[string]any{"name": n, "model": n, "size": 1 << 30})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (r *Runtime) handlePS(w http.ResponseWriter, _ *http.Request) {
	models := []map[string]any{}
	for _, n := range r.Loaded() {
		models = append(models, map[string]any{"name": n, "model": n, "size_vram": 1 << 30})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (r *Runtime) handlePull(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Model string `json:"model"`
		Name  string `json:"name"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	model := body.Model
	if model == "" {
		model = body.Name
	}
	cfg := r.config()

	// Ollama answers 200 and reports failures inside the stream.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	line := func(v any) {
		_ = json.NewEncoder(w).Encode(v)
		if flusher != nil {
			flusher.Flush()
		}
	}

	line(map[string]any{"status": "pulling manifest"})
	if model == "" || slices.Contains(cfg.FailPull, model) {
		line(map[string]any{"error": fmt.Sprintf("pull model manifest: file does not exist (%s)", model)})
		return
	}
	steps, pause := cfg.PullSteps, cfg.PullStepMs
	if steps <= 0 {
		steps = 3
	}
	if pause <= 0 {
		pause = 20
	}
	const total = int64(1) << 30
	for i := 1; i <= steps; i++ {
		select {
		case <-req.Context().Done():
			return
		case <-time.After(time.Duration(pause) * time.Millisecond):
		}
		line(map[string]any{"status": "pulling 0123456789ab", "digest": "sha256:0123456789ab", "total": total, "completed": total * int64(i) / int64(steps)})
	}
	line(map[string]any{"status": "verifying sha256 digest"})
	r.mu.Lock()
	r.pulled[model] = true
	r.mu.Unlock()
	line(map[string]any{"status": "success"})
}

// handleGenerate implements the one use the agent makes of /api/generate: an
// empty prompt with keep_alive, which loads (non-zero) or unloads (zero) a model.
func (r *Runtime) handleGenerate(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Model     string `json:"model"`
		KeepAlive *int64 `json:"keep_alive"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	cfg := r.config()
	r.mu.Lock()
	known := r.pulled[body.Model]
	r.mu.Unlock()
	if !known {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("model '%s' not found", body.Model)})
		return
	}
	unload := body.KeepAlive != nil && *body.KeepAlive == 0
	if !unload && slices.Contains(cfg.FailLoad, body.Model) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "model requires more system memory than is available"})
		return
	}
	r.mu.Lock()
	if unload {
		delete(r.loaded, body.Model)
	} else {
		r.loaded[body.Model] = true
	}
	r.mu.Unlock()
	reason := "load"
	if unload {
		reason = "unload"
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": body.Model, "response": "", "done": true, "done_reason": reason})
}

func (r *Runtime) handleDelete(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Model string `json:"model"`
		Name  string `json:"name"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	model := body.Model
	if model == "" {
		model = body.Name
	}
	r.mu.Lock()
	known := r.pulled[model]
	delete(r.pulled, model)
	delete(r.loaded, model)
	r.mu.Unlock()
	if !known {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("model '%s' not found", model)})
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ─── OpenAI-compatible API ────────────────────────────────────

func (r *Runtime) handleModels(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	names := sortedKeys(r.pulled)
	r.mu.Unlock()
	data := []map[string]any{}
	for _, n := range names {
		data = append(data, map[string]any{"id": n, "object": "model", "created": 0, "owned_by": "library"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func openAIError(w http.ResponseWriter, status int, kind, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": kind, "param": nil, "code": nil}})
}

// words is the fixed reply; token i is words[i % len(words)].
var words = strings.Fields("hello from the fake runtime which answers with the same words every time")

type completionRequest struct {
	Model     string `json:"model"`
	Stream    bool   `json:"stream"`
	MaxTokens *int   `json:"max_tokens"`
	// MaxCompletionTokens is the newer name for the same limit.
	MaxCompletionTokens *int `json:"max_completion_tokens"`
	StreamOptions       struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Messages []struct {
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Prompt json.RawMessage `json:"prompt"`
}

func (r *Runtime) handleCompletion(w http.ResponseWriter, req *http.Request, chat bool) {
	var body completionRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		openAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	cfg := r.config()
	r.mu.Lock()
	known := r.pulled[body.Model]
	r.mu.Unlock()
	if !known {
		openAIError(w, http.StatusNotFound, "not_found_error", fmt.Sprintf("model '%s' not found", body.Model))
		return
	}
	if cfg.Hang {
		<-req.Context().Done()
		return
	}
	if cfg.FirstByteDelayMs > 0 {
		select {
		case <-req.Context().Done():
			return
		case <-time.After(time.Duration(cfg.FirstByteDelayMs) * time.Millisecond):
		}
	}
	if cfg.Status != 0 {
		openAIError(w, cfg.Status, "api_error", "fake runtime was told to fail")
		return
	}
	// Answering a model loads it, as it does in Ollama.
	r.mu.Lock()
	r.loaded[body.Model] = true
	r.mu.Unlock()

	n := cfg.Tokens
	if n <= 0 {
		n = 8
	}
	limit := body.MaxTokens
	if limit == nil {
		limit = body.MaxCompletionTokens
	}
	finish := "stop"
	if limit != nil && *limit >= 0 && *limit < n {
		n, finish = *limit, "length"
	}
	chars := len(body.Prompt)
	for _, m := range body.Messages {
		chars += len(m.Content)
	}
	usage := map[string]int{"prompt_tokens": (chars + 3) / 4, "completion_tokens": n, "total_tokens": (chars+3)/4 + n}

	id, object, created := "chatcmpl-fake", "chat.completion", time.Now().Unix()
	if !chat {
		id, object = "cmpl-fake", "text_completion"
	}
	token := func(i int) string {
		if i == 0 {
			return words[0]
		}
		return " " + words[i%len(words)]
	}

	if !body.Stream {
		var text strings.Builder
		for i := 0; i < n; i++ {
			text.WriteString(token(i))
		}
		choice := map[string]any{"index": 0, "finish_reason": finish}
		if chat {
			choice["message"] = map[string]any{"role": "assistant", "content": text.String()}
		} else {
			choice["text"] = text.String()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": object, "created": created, "model": body.Model,
			"choices": []any{choice}, "usage": usage,
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	event := func(choices []any, extra map[string]any) {
		chunk := map[string]any{"id": id, "object": object + ".chunk", "created": created, "model": body.Model, "choices": choices}
		if !chat {
			chunk["object"] = object
		}
		for k, v := range extra {
			chunk[k] = v
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	for i := 0; i < n; i++ {
		if cfg.TokenDelayMs > 0 {
			select {
			case <-req.Context().Done():
				return
			case <-time.After(time.Duration(cfg.TokenDelayMs) * time.Millisecond):
			}
		}
		if chat {
			delta := map[string]any{"content": token(i)}
			if i == 0 {
				delta["role"] = "assistant"
			}
			event([]any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}, nil)
		} else {
			event([]any{map[string]any{"index": 0, "text": token(i), "finish_reason": nil}}, nil)
		}
	}
	if chat {
		event([]any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, nil)
	} else {
		event([]any{map[string]any{"index": 0, "text": "", "finish_reason": finish}}, nil)
	}
	if body.StreamOptions.IncludeUsage {
		event([]any{}, map[string]any{"usage": usage})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func (r *Runtime) handleEmbeddings(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		openAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	r.mu.Lock()
	known := r.pulled[body.Model]
	r.mu.Unlock()
	if !known {
		openAIError(w, http.StatusNotFound, "not_found_error", fmt.Sprintf("model '%s' not found", body.Model))
		return
	}
	var inputs []string
	if err := json.Unmarshal(body.Input, &inputs); err != nil {
		var one string
		if err := json.Unmarshal(body.Input, &one); err != nil {
			openAIError(w, http.StatusBadRequest, "invalid_request_error", "input must be a string or a list of strings")
			return
		}
		inputs = []string{one}
	}
	data := []map[string]any{}
	tokens := 0
	for i, in := range inputs {
		// A small deterministic vector derived from the text.
		vec := make([]float64, 8)
		for j, c := range []byte(in) {
			vec[j%len(vec)] += float64(c) / 255
		}
		data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": vec})
		tokens += (len(in) + 3) / 4
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list", "data": data, "model": body.Model,
		"usage": map[string]int{"prompt_tokens": tokens, "total_tokens": tokens},
	})
}
