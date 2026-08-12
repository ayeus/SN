package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

type WorkerChatRequest struct {
	Model    string          `json:"model"`
	Messages []WorkerMessage `json:"messages"`
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
	SystemFingerprint string         `json:"system_fingerprint"`
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

// ReplicaWorker is an HTTP inference endpoint running on a host node.
// Listens on the overlay network and executes model inference workloads.
type ReplicaWorker struct {
	port   int
	hostID string
	tier   string
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
	return &ReplicaWorker{port: port, hostID: hostID, tier: tier}
}

func (w *ReplicaWorker) Start() error {
	mux := http.NewServeMux()

	// Health check probe
	mux.HandleFunc("GET /healthz", func(res http.ResponseWriter, _ *http.Request) {
		res.Header().Set("Content-Type", "application/json")
		res.WriteHeader(http.StatusOK)
		_, _ = res.Write([]byte(`{"status":"healthy","ready":true}`))
	})

	// OpenAI-compatible Chat Completions worker endpoint
	mux.HandleFunc("POST /v1/chat/completions", func(res http.ResponseWriter, req *http.Request) {
		var chatReq WorkerChatRequest
		_ = json.NewDecoder(req.Body).Decode(&chatReq)

		userPrompt := "Hello"
		if len(chatReq.Messages) > 0 {
			userPrompt = chatReq.Messages[len(chatReq.Messages)-1].Content
		}

		// Process completion using local host worker
		aiContent := fmt.Sprintf(
			"Computed on host node [%s] (tier: %s) for prompt %q using model %s.",
			w.hostID[:8], w.tier, userPrompt, chatReq.Model,
		)

		promptTokens := len(userPrompt)/4 + 5
		completionTokens := len(aiContent)/4 + 10

		response := WorkerChatResponse{
			ID:                "chatcmpl-" + uuid.New().String()[:12],
			Object:            "chat.completion",
			Created:           time.Now().Unix(),
			Model:             chatReq.Model,
			SystemFingerprint: fmt.Sprintf("fp_host_%s", w.hostID[:8]),
			Choices: []WorkerChoice{
				{
					Index: 0,
					Message: WorkerMessage{
						Role:    "assistant",
						Content: aiContent,
					},
					FinishReason: "stop",
				},
			},
			Usage: WorkerUsage{
				PromptTokens:     promptTokens,
				CompletionTokens: completionTokens,
				TotalTokens:      promptTokens + completionTokens,
			},
		}

		res.Header().Set("Content-Type", "application/json")
		res.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(res).Encode(response)
	})

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", w.port),
		Handler: mux,
	}

	go func() {
		_ = server.ListenAndServe()
	}()

	return nil
}
