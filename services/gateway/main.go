// Package main implements the AyeusANN API Gateway.
// Responsibilities: AuthN, rate limiting, request routing to internal services.
package main

import (
	"log"
	"net/http"
	"strconv"

	"github.com/ayeus/ayeusann/internal/platform"
)

const (
	serviceName    = "gateway"
	serviceVersion = "0.1.0"
)

func main() {
	port, _ := strconv.Atoi(platform.MustEnv("GATEWAY_PORT", "8080"))

	srv, err := platform.NewServer(platform.ServiceConfig{
		Name:    serviceName,
		Version: serviceVersion,
		Port:    port,
	})
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	srv.Mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"AyeusANN-gateway","version":"0.1.0"}`))
	})

	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
