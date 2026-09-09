// Package main implements the AyeusANN Inference Gateway.
// Responsibilities: API key auth, rate limiting, OpenAI wire format, request routing, usage emission.
package main

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/config"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/platform"
)

const devServiceSecret = "dev-only-insecure-internal-service-key-0001"

func main() {
	port, _ := strconv.Atoi(platform.MustEnv("INFERENCE_GW_PORT", "8085"))
	dbURL := platform.MustEnv("DATABASE_URL", "postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann?sslmode=disable")
	routerURL := platform.MustEnv("ROUTER_URL", "http://localhost:8084")
	billingURL := platform.MustEnv("BILLING_URL", "http://localhost:8086")

	serviceSecret, err := platform.RequireSecret("INTERNAL_SERVICE_SECRET", devServiceSecret, 32)
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	svcAuth, err := auth.NewServiceAuthenticator(serviceSecret, auth.ServiceInferenceGateway)
	if err != nil {
		log.Fatalf("failed to create service authenticator: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbClient, err := db.NewClient(ctx, db.Config{URL: dbURL})
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer dbClient.Close()

	// Set up Redis rate limiter (fail-open if Redis unavailable)
	var rateLimiter *RateLimiter
	rdb, err := config.NewRedisClient()
	if err != nil {
		log.Printf("WARNING: Redis unavailable, rate limiting disabled: %v", err)
	} else {
		if pingErr := rdb.Ping(ctx).Err(); pingErr != nil {
			log.Printf("WARNING: Redis ping failed, rate limiting disabled: %v", pingErr)
		} else {
			rateLimiter = NewRateLimiter(rdb, 60) // 60 RPM default
			log.Println("Redis rate limiter enabled (60 RPM per API key)")
		}
	}

	handler := NewInferenceHandler(dbClient, rateLimiter, routerURL, billingURL, svcAuth)

	srv, err := platform.NewServer(platform.ServiceConfig{
		Name:    "inference-gateway",
		Version: "0.2.0",
		Port:    port,
	})
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	// OpenAI-compatible endpoints
	srv.Mux.HandleFunc("POST /v1/chat/completions", handler.HandleChatCompletions)
	srv.Mux.HandleFunc("OPTIONS /v1/chat/completions", handler.HandleChatCompletions)
	srv.Mux.HandleFunc("GET /v1/models", handler.HandleListModels)

	srv.Mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"AyeusANN-inference-gateway","version":"0.2.0"}`))
	})

	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

