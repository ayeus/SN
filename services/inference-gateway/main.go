// Package main implements the AyeusANN Inference Gateway.
// Responsibilities: TLS, key auth, per-key limits, OpenAI wire format endpoints.
package main

import (
	"log"
	"net/http"
	"strconv"

	"github.com/ayeus/ayeusann/internal/platform"
)

func main() {
	port, _ := strconv.Atoi(platform.MustEnv("INFERENCE_GW_PORT", "8085"))

	srv, err := platform.NewServer(platform.ServiceConfig{
		Name:    "inference-gateway",
		Version: "0.1.0",
		Port:    port,
	})
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	handler := NewInferenceHandler()

	// OpenAI-compatible Chat Completions endpoint
	srv.Mux.HandleFunc("POST /v1/chat/completions", handler.HandleChatCompletions)
	srv.Mux.HandleFunc("OPTIONS /v1/chat/completions", handler.HandleChatCompletions)

	srv.Mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"AyeusANN-inference-gateway","version":"0.1.0"}`))
	})

	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
