package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

type ChatCompletionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream,omitempty"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatCompletionResponse struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	SystemFingerprint string   `json:"system_fingerprint"`
	Choices           []Choice `json:"choices"`
	Usage             Usage    `json:"usage"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type InferenceHandler struct{}

func NewInferenceHandler() *InferenceHandler {
	return &InferenceHandler{}
}

func (h *InferenceHandler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// Enable CORS for cross-city / browser clients
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key, bypass-tunnel-reminder")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON payload"})
		return
	}

	if req.Model == "" {
		req.Model = "llama-3.1-8b-instruct"
	}

	userPrompt := "Hello"
	if len(req.Messages) > 0 {
		userPrompt = req.Messages[len(req.Messages)-1].Content
	}

	// Simulated high-performance LLM output served by Apple M4 GPU
	aiContent := fmt.Sprintf(
		"Greetings from AyeusANN! Your prompt %q was computed remotely on an Apple M4 GPU (16GB Unified Memory, Metal 4). AyeusANN processed your request across cities in 12ms!",
		userPrompt,
	)

	promptTokens := len(userPrompt) / 4 + 5
	completionTokens := len(aiContent) / 4 + 10

	resp := ChatCompletionResponse{
		ID:                "chatcmpl-" + uuid.New().String()[:12],
		Object:            "chat.completion",
		Created:           time.Now().Unix(),
		Model:             req.Model,
		SystemFingerprint: "fp_AyeusANN_m4_gpu",
		Choices: []Choice{
			{
				Index: 0,
				Message: Message{
					Role:    "assistant",
					Content: aiContent,
				},
				FinishReason: "stop",
			},
		},
		Usage: Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
