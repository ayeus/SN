// Package main implements the AyeusANN Inference Gateway.
//
// Architecture §4: TLS termination, key auth, per-key rate limits — plus the
// request router (replica selection, health, retries) embedded per ADR-011.
// Requests reach hosts through the coordinator's tunnel; every request is
// recorded as a usage event.
package main

import (
	"context"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/config"
	"github.com/ayeus/ayeusann/internal/platform"
)

func main() {
	port := platform.EnvInt("INFERENCE_GW_PORT", 8085)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "inference-gateway")

	jwtSecret, err := platform.JWTSecret()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}
	internalSecret, err := platform.InternalSecret()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}
	svcAuth, err := auth.NewServiceAuthenticator(internalSecret, auth.ServiceInferenceGateway)
	if err != nil {
		log.Fatalf("failed to create service authenticator: %v", err)
	}
	tm, err := auth.NewTokenManager(jwtSecret, 15*time.Minute, 7*24*time.Hour)
	if err != nil {
		log.Fatalf("failed to create token manager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbClient, err := platform.ConnectDB(ctx)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer dbClient.Close()

	// Per-key rate limiting (SRS FR-31). Without Redis the gateway still serves,
	// but says so loudly; in production a missing limiter is a startup error.
	var limiter *RateLimiter
	if rdb, err := config.NewRedisClient(); err == nil && rdb.Ping(ctx).Err() == nil {
		limiter = NewRateLimiter(rdb, platform.EnvInt("RATE_LIMIT_RPM", 60))
		logger.Info("rate limiter enabled", "rpm", platform.EnvInt("RATE_LIMIT_RPM", 60))
	} else if platform.IsProduction() {
		log.Fatalf("configuration error: Redis is required for rate limiting in production")
	} else {
		logger.Warn("Redis unavailable; rate limiting disabled in development")
	}

	g := &Gateway{
		db:             dbClient,
		tm:             tm,
		revocations:    auth.NewPGRevocationStore(dbClient.Pool),
		limiter:        limiter,
		router:         newRouter(dbClient, platform.HeartbeatTimeout()),
		svcAuth:        svcAuth,
		coordinatorURL: platform.CoordinatorURL(),
		inferenceHost:  platform.Env("INFERENCE_HOST", ""),
		log:            logger,
		client: &http.Client{
			// No overall timeout: streams legitimately last minutes. The
			// coordinator enforces per-request deadlines.
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				ResponseHeaderTimeout: 70 * time.Second,
				MaxIdleConnsPerHost:   64,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}

	srv, err := platform.NewServer(platform.ServiceConfig{Name: "inference-gateway", Version: platform.Version, Port: port})
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	srv.Mux.HandleFunc("POST /v1/chat/completions", g.HandleInference)
	srv.Mux.HandleFunc("POST /v1/completions", g.HandleInference)
	srv.Mux.HandleFunc("POST /v1/embeddings", g.HandleInference)
	srv.Mux.HandleFunc("GET /v1/models", g.HandleListModels)

	srv.SetReadyCheck(func(ctx context.Context) error { return dbClient.Pool.Ping(ctx) })
	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
