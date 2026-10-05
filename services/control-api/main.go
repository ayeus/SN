// Package main implements the AyeusANN Control API: organisations, users, API
// keys, the model catalogue, deployments, hosts and the ops console
// (Architecture §4). It does not serve the frontend; the gateway does.
package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/httpx"
	"github.com/ayeus/ayeusann/internal/platform"
)

func main() {
	port := platform.EnvInt("CONTROL_API_PORT", 8081)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "control-api")

	jwtSecret, err := platform.JWTSecret()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	ctx := context.Background()
	dbClient, err := platform.ConnectDB(ctx)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer dbClient.Close()

	tm, err := auth.NewTokenManager(jwtSecret, 15*time.Minute, 7*24*time.Hour)
	if err != nil {
		log.Fatalf("failed to initialize token manager: %v", err)
	}

	api := NewAPI(dbClient, tm, auth.NewPGRevocationStore(dbClient.Pool), logger, Config{
		PublicURL:            platform.PublicURL(),
		CoordinatorPublicURL: platform.CoordinatorPublicURL(),
		InferenceHost:        platform.Env("INFERENCE_HOST", ""),
		HeartbeatTimeout:     platform.HeartbeatTimeout(),
		PlatformAdminEmails:  httpx.SplitList(platform.Env("PLATFORM_ADMIN_EMAILS", "")),
	})

	srv, err := platform.NewServer(platform.ServiceConfig{Name: "control-api", Version: "0.3.0", Port: port})
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}
	srv.Mux.Handle("/v1/", api.Routes())
	srv.SetReadyCheck(func(ctx context.Context) error { return dbClient.Pool.Ping(ctx) })
	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func platformMetrics(h http.Handler) http.Handler {
	return platform.MetricsMiddleware("control-api", h)
}
