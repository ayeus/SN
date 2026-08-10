// Package main implements the SpazeNode Scheduler.
// Responsibilities: GPU placement (filter→score→reserve), rebalance loops.
package main

import (
	"log"
	"net/http"
	"strconv"

	"github.com/spazor/spazenode/internal/platform"
)

func main() {
	port, _ := strconv.Atoi(platform.MustEnv("SCHEDULER_PORT", "8082"))

	srv, err := platform.NewServer(platform.ServiceConfig{
		Name:    "scheduler",
		Version: "0.1.0",
		Port:    port,
	})
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	srv.Mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"spazenode-scheduler","version":"0.1.0"}`))
	})

	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
