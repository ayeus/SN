// Package main implements the AyeusANN Trust Engine.
// Responsibilities: reputation scoring, sampled re-execution, incident tracking.
package main

import (
	"log"
	"net/http"
	"strconv"

	"github.com/ayeus/ayeusann/internal/platform"
)

func main() {
	port, _ := strconv.Atoi(platform.MustEnv("TRUST_PORT", "8087"))

	srv, err := platform.NewServer(platform.ServiceConfig{
		Name:    "trust-engine",
		Version: "0.1.0",
		Port:    port,
	})
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	srv.Mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"AyeusANN-trust-engine","version":"0.1.0"}`))
	})

	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
