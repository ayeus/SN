// Package main implements the AyeusANN Request Router.
// Responsibilities: replica selection, health checks, retries, tier policy.
package main

import (
	"log"
	"net/http"
	"strconv"

	"github.com/ayeus/ayeusann/internal/platform"
)

func main() {
	port, _ := strconv.Atoi(platform.MustEnv("ROUTER_PORT", "8084"))

	srv, err := platform.NewServer(platform.ServiceConfig{
		Name:    "router",
		Version: "0.1.0",
		Port:    port,
	})
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	srv.Mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"AyeusANN-router","version":"0.1.0"}`))
	})

	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
