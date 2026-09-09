// Package main implements the AyeusANN Control API.
// Responsibilities: Auth, CRUD for orgs, users, API keys, models, hosts, deployments.
package main

import (
	"context"
	"log"
	"net/http"
	"path/filepath"
	"time"

	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/platform"
)

// devJWTSecret is used only when SN_ENV is dev/development/test. In any other
// environment platform.RequireSecret refuses it and the service will not start.
const devJWTSecret = "dev-only-insecure-jwt-signing-key-0001"

func main() {
	port := platform.EnvInt("CONTROL_API_PORT", 8081)

	dbURL, err := platform.RequireEnv("DATABASE_URL",
		"postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann?sslmode=disable")
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	// A weak or missing signing key is a total authentication bypass, so the
	// service refuses to start rather than falling back to a known default.
	jwtSecret, err := platform.RequireSecret("JWT_SECRET", devJWTSecret, 32)
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	// Initialize DB Client
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbClient, err := db.NewClient(ctx, db.Config{URL: dbURL})
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer dbClient.Close()

	// Initialize TokenManager & LedgerService
	tm, err := auth.NewTokenManager(jwtSecret, 15*time.Minute, 7*24*time.Hour)
	if err != nil {
		log.Fatalf("failed to initialize token manager: %v", err)
	}
	revocations := auth.NewPGRevocationStore(dbClient.Pool)
	authMW := auth.NewMiddleware(tm, revocations)
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

	authHandler := NewAuthHandler(dbClient, tm, revocations, ledger)
	modelHandler := NewModelHandler(dbClient)
	hostHandler := NewHostHandler(dbClient, tm)

	// Public Auth Endpoints
	srv.Mux.HandleFunc("POST /v1/auth/signup", authHandler.HandleSignup)
	srv.Mux.HandleFunc("POST /v1/auth/login", authHandler.HandleLogin)
	srv.Mux.HandleFunc("POST /v1/auth/refresh", authHandler.HandleRefresh)

	// Public Model Catalog Endpoints
	srv.Mux.HandleFunc("GET /v1/models", modelHandler.HandleListModels)
	srv.Mux.HandleFunc("GET /v1/models/{id}", modelHandler.HandleGetModel)

	// Protected Endpoints
	protectedMux := http.NewServeMux()
	protectedMux.HandleFunc("GET /v1/auth/me", authHandler.HandleMe)
	protectedMux.HandleFunc("POST /v1/auth/logout", authHandler.HandleLogout)
	protectedMux.HandleFunc("POST /v1/auth/logout-all", authHandler.HandleLogoutAll)
	protectedMux.Handle("POST /v1/api-keys", auth.RequireRole("admin", "member")(http.HandlerFunc(authHandler.HandleCreateAPIKey)))
	protectedMux.Handle("GET /v1/api-keys", auth.RequireRole("admin", "member")(http.HandlerFunc(authHandler.HandleListAPIKeys)))
	protectedMux.Handle("DELETE /v1/api-keys/{id}", auth.RequireRole("admin")(http.HandlerFunc(authHandler.HandleRevokeAPIKey)))
	protectedMux.Handle("POST /v1/models/byo", auth.RequireRole("admin", "member")(http.HandlerFunc(modelHandler.HandleBYOModel)))

	// Host Management Endpoints
	protectedMux.Handle("POST /v1/hosts/register-token", auth.RequireRole("admin", "member")(http.HandlerFunc(hostHandler.HandleIssueRegistrationToken)))
	protectedMux.HandleFunc("GET /v1/hosts", hostHandler.HandleListHosts)
	protectedMux.HandleFunc("GET /v1/hosts/{id}", hostHandler.HandleGetHost)

	// Everything under the protected mux goes through authentication once, here,
	// rather than being trusted because it happened to be registered on a
	// different mux.
	protected := authMW.Authenticate(protectedMux)

	// Serve Static Frontend UI & Installer Scripts. Resolving the directory at
	// startup means the service no longer depends on being launched from the
	// repository root.
	webDir := platform.Env("WEB_DIR", "web")
	fileServer := http.FileServer(http.Dir(webDir))
	srv.Mux.Handle("GET /static/", http.StripPrefix("/static/", fileServer))
	srv.Mux.HandleFunc("GET /install.sh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		http.ServeFile(w, r, filepath.Join(webDir, "install.sh"))
	})
	srv.Mux.HandleFunc("GET /install.ps1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.ServeFile(w, r, filepath.Join(webDir, "install.ps1"))
	})
	srv.Mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			http.ServeFile(w, r, filepath.Join(webDir, "index.html"))
			return
		}
		protected.ServeHTTP(w, r)
	})
	// Non-GET traffic that did not match a public route still needs to reach the
	// protected mux; "GET /" only matches GET.
	srv.Mux.Handle("/", protected)

	// Readiness reflects the database, so a replica with a dead pool is pulled
	// from the load balancer instead of serving 500s.
	srv.SetReadyCheck(func(ctx context.Context) error {
		return dbClient.Pool.Ping(ctx)
	})

	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
