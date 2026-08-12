// Package main implements the AyeusANN Control API.
// Responsibilities: Auth, CRUD for orgs, users, API keys, models, hosts, deployments.
package main

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/platform"
)

func main() {
	port, _ := strconv.Atoi(platform.MustEnv("CONTROL_API_PORT", "8081"))
	dbURL := platform.MustEnv("DATABASE_URL", "postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann?sslmode=disable")
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
	modelHandler := NewModelHandler(dbClient)
	hostHandler := NewHostHandler(dbClient, tm)

	// Public Auth Endpoints
	srv.Mux.HandleFunc("POST /v1/auth/signup", authHandler.HandleSignup)
	srv.Mux.HandleFunc("POST /v1/auth/login", authHandler.HandleLogin)

	// Public Model Catalog Endpoints
	srv.Mux.HandleFunc("GET /v1/models", modelHandler.HandleListModels)
	srv.Mux.HandleFunc("GET /v1/models/", modelHandler.HandleGetModel)

	// Protected Endpoints
	protectedMux := http.NewServeMux()
	protectedMux.HandleFunc("GET /v1/auth/me", authHandler.HandleMe)
	protectedMux.Handle("POST /v1/api-keys", auth.RequireRole("admin", "member")(http.HandlerFunc(authHandler.HandleCreateAPIKey)))
	protectedMux.Handle("POST /v1/models/byo", auth.RequireRole("admin", "member")(http.HandlerFunc(modelHandler.HandleBYOModel)))

	// Host Management Endpoints
	protectedMux.Handle("POST /v1/hosts/register-token", auth.RequireRole("admin", "member")(http.HandlerFunc(hostHandler.HandleIssueRegistrationToken)))
	protectedMux.HandleFunc("GET /v1/hosts", hostHandler.HandleListHosts)
	protectedMux.HandleFunc("GET /v1/hosts/", hostHandler.HandleGetHost)

	// Serve Static Frontend UI & Installer Scripts
	fs := http.FileServer(http.Dir("web"))
	srv.Mux.Handle("GET /static/", http.StripPrefix("/static/", fs))
	srv.Mux.HandleFunc("GET /install.sh", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "web/install.sh")
	})
	srv.Mux.HandleFunc("GET /install.ps1", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "web/install.ps1")
	})
	srv.Mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			http.ServeFile(w, r, "web/index.html")
			return
		}
		if r.URL.Path == "/install.sh" {
			http.ServeFile(w, r, "web/install.sh")
			return
		}
		if r.URL.Path == "/install.ps1" {
			http.ServeFile(w, r, "web/install.ps1")
			return
		}
		tm.AuthMiddleware(protectedMux).ServeHTTP(w, r)
	})

	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
