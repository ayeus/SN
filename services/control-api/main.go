// Package main implements the SpazeNode Control API.
// Responsibilities: Auth, CRUD for orgs, users, API keys, models, deployments.
package main

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/spazor/spazenode/internal/auth"
	"github.com/spazor/spazenode/internal/db"
	"github.com/spazor/spazenode/internal/platform"
)

func main() {
	port, _ := strconv.Atoi(platform.MustEnv("CONTROL_API_PORT", "8081"))
	dbURL := platform.MustEnv("DATABASE_URL", "postgres://spazenode:spazenode_dev@localhost:5433/spazenode?sslmode=disable")
	jwtSecret := platform.MustEnv("JWT_SECRET", "dev-secret-key-32-bytes-long-super-secure!")

	// Initialize DB Client
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbClient, err := db.NewClient(ctx, db.Config{URL: dbURL})
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer dbClient.Close()

	// Initialize TokenManager & LedgerService
	tm := auth.NewTokenManager(jwtSecret, 15*time.Minute, 7*24*time.Hour)
	ledger := db.NewLedgerService(dbClient)

	// Initialize Server
	srv, err := platform.NewServer(platform.ServiceConfig{
		Name:    "control-api",
		Version: "0.1.0",
		Port:    port,
	})
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	authHandler := NewAuthHandler(dbClient, tm, ledger)

	// Public Auth Endpoints
	srv.Mux.HandleFunc("POST /v1/auth/signup", authHandler.HandleSignup)
	srv.Mux.HandleFunc("POST /v1/auth/login", authHandler.HandleLogin)

	// Protected Endpoints
	protectedMux := http.NewServeMux()
	protectedMux.HandleFunc("GET /v1/auth/me", authHandler.HandleMe)
	protectedMux.Handle("POST /v1/api-keys", auth.RequireRole("admin", "member")(http.HandlerFunc(authHandler.HandleCreateAPIKey)))

	srv.Mux.Handle("/", tm.AuthMiddleware(protectedMux))

	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
