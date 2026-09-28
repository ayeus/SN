package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// WorkerChatRequest represents an OpenAI-compatible completion request payload.
type WorkerChatRequest struct {
	Model       string          `json:"model"`
	Messages    []WorkerMessage `json:"messages"`
	Stream      bool            `json:"stream,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
}

type WorkerMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type WorkerChatResponse struct {
	ID                string         `json:"id"`
	Object            string         `json:"object"`
	Created           int64          `json:"created"`
	Model             string         `json:"model"`
	SystemFingerprint string         `json:"system_fingerprint,omitempty"`
	Choices           []WorkerChoice `json:"choices"`
	Usage             WorkerUsage    `json:"usage"`
}

type WorkerChoice struct {
	Index        int           `json:"index"`
	Message      WorkerMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type WorkerUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// InferenceEngine is the adapter abstraction for executing real model workloads.
type InferenceEngine interface {
	Generate(ctx context.Context, req WorkerChatRequest) (*WorkerChatResponse, error)
	Health(ctx context.Context) error
}

// HTTPInferenceEngine connects to a real inference engine (e.g. Ollama, vLLM, or OpenAI-compatible backend).
type HTTPInferenceEngine struct {
	endpoint   string
	apiKey     string
	httpClient *http.Client
}

// NewHTTPInferenceEngine creates an inference engine connecting to the configured HTTP backend.
func NewHTTPInferenceEngine(endpoint, apiKey string) *HTTPInferenceEngine {
	if endpoint == "" {
		endpoint = os.Getenv("INFERENCE_BACKEND_ENDPOINT")
	}
	if endpoint == "" {
		// Default to local Ollama API if running, otherwise standard local vLLM port
		endpoint = "http://127.0.0.1:11434/v1"
	}
	endpoint = strings.TrimRight(endpoint, "/")

	if apiKey == "" {
		apiKey = os.Getenv("INFERENCE_BACKEND_API_KEY")
	}

	return &HTTPInferenceEngine{
		endpoint: endpoint,
		apiKey:   apiKey,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

func (e *HTTPInferenceEngine) Health(ctx context.Context) error {
	reqURL := e.endpoint + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("engine unreachable at %s: %w", e.endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("engine health check failed with status %d", resp.StatusCode)
	}
	return nil
}

func (e *HTTPInferenceEngine) Generate(ctx context.Context, req WorkerChatRequest) (*WorkerChatResponse, error) {
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	reqURL := e.endpoint + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create engine request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("inference backend unavailable (%s): %w", e.endpoint, err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("inference backend error (%d): %s", resp.StatusCode, string(bodyBytes))
	}

	var chatResp WorkerChatResponse
	if err := json.Unmarshal(bodyBytes, &chatResp); err != nil {
		return nil, fmt.Errorf("failed to decode backend response: %w", err)
	}

	if chatResp.ID == "" {
		chatResp.ID = "chatcmpl-" + uuid.New().String()[:12]
	}
	if chatResp.Object == "" {
		chatResp.Object = "chat.completion"
	}
	if chatResp.Created == 0 {
		chatResp.Created = time.Now().Unix()
	}

	return &chatResp, nil
}

// ReplicaWorker is an HTTP inference endpoint running on a host node.
type ReplicaWorker struct {
	port   int
	hostID string
	tier   string
	engine InferenceEngine
}

func NewReplicaWorker(port int, hostID, tier string) *ReplicaWorker {
	if port <= 0 {
		port = 8000
	}
	if hostID == "" {
		hostID = "local-host-node"
	}
	if tier == "" {
		tier = "t2"
	}

	engine := NewHTTPInferenceEngine("", "")
	return &ReplicaWorker{
		port:   port,
		hostID: hostID,
		tier:   tier,
		engine: engine,
	}
}

// SetEngine overrides the inference engine (useful for testing or specialized model backends).
func (w *ReplicaWorker) SetEngine(engine InferenceEngine) {
	w.engine = engine
}

func (w *ReplicaWorker) Start() error {
	mux := http.NewServeMux()

	// Health check probe
	mux.HandleFunc("GET /healthz", func(res http.ResponseWriter, req *http.Request) {
		res.Header().Set("Content-Type", "application/json")
		ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()

		engineHealthy := true
		var engineErr string
		if err := w.engine.Health(ctx); err != nil {
			engineHealthy = false
			engineErr = err.Error()
		}

		status := http.StatusOK
		statusText := "healthy"
		if !engineHealthy {
			// Report degraded health if engine is currently offline
			status = http.StatusServiceUnavailable
			statusText = "degraded"
		}

		res.WriteHeader(status)
		_ = json.NewEncoder(res).Encode(map[string]interface{}{
			"status":         statusText,
			"host_id":        w.hostID,
			"tier":           w.tier,
			"engine_healthy": engineHealthy,
			"engine_error":   engineErr,
		})
	})

	// OpenAI-compatible Chat Completions worker endpoint
	mux.HandleFunc("POST /v1/chat/completions", func(res http.ResponseWriter, req *http.Request) {
		var chatReq WorkerChatRequest
		if err := json.NewDecoder(req.Body).Decode(&chatReq); err != nil {
			res.Header().Set("Content-Type", "application/json")
			res.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(res).Encode(map[string]string{"error": "Invalid request payload: " + err.Error()})
			return
		}

		if len(chatReq.Messages) == 0 {
			res.Header().Set("Content-Type", "application/json")
			res.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(res).Encode(map[string]string{"error": "Messages array cannot be empty"})
			return
		}

		// Forward to the real inference engine adapter
		resp, err := w.engine.Generate(req.Context(), chatReq)
		if err != nil {
			log.Printf("worker %s: inference error: %v", w.hostID, err)
			res.Header().Set("Content-Type", "application/json")
			res.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(res).Encode(map[string]string{
				"error": fmt.Sprintf("Inference engine error: %v. Please ensure an active model backend (Ollama, vLLM, etc.) is reachable.", err),
			})
			return
		}

		res.Header().Set("Content-Type", "application/json")
		res.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(res).Encode(resp)
	})

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", w.port),
		Handler: mux,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("replica worker on port %d exited: %v", w.port, err)
		}
	}()

	return nil
}
