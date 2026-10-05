package platform

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ayeus/ayeusann/internal/db"
)

// Development defaults. Every service reads them through the helpers below, so
// services started without a .env agree with each other. Before this, the
// control-api and scheduler defaulted to one internal secret while the router
// and inference-gateway defaulted to another, so every signed
// service-to-service call failed under `make dev`. RequireSecret refuses both
// values outside dev/test.
const (
	DevJWTSecret      = "dev-only-insecure-jwt-signing-key-0001"
	DevInternalSecret = "dev-only-insecure-internal-service-key-0001"
	DevDatabaseURL    = "postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann_dev?sslmode=disable"
)

// JWTSecret returns the HMAC key for user and host-registration tokens.
func JWTSecret() (string, error) {
	return RequireSecret("JWT_SECRET", DevJWTSecret, 32)
}

// InternalSecret returns the shared key for service-to-service credentials.
func InternalSecret() (string, error) {
	return RequireSecret("INTERNAL_SERVICE_SECRET", DevInternalSecret, 32)
}

// DatabaseURL returns the Postgres connection string.
func DatabaseURL() (string, error) {
	return RequireEnv("DATABASE_URL", DevDatabaseURL)
}

// ConnectDB opens the pool, retrying briefly so services started alongside
// Postgres do not crash-loop while it boots.
func ConnectDB(ctx context.Context) (*db.Client, error) {
	url, err := DatabaseURL()
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		client, err := db.NewClient(cctx, db.Config{URL: url})
		cancel()
		if err == nil {
			return client, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, fmt.Errorf("platform: database unreachable: %w", lastErr)
}

// Service URLs. Inside docker-compose these are overridden with container names.
func ControlAPIURL() string  { return Env("CONTROL_API_URL", "http://localhost:8081") }
func SchedulerURL() string   { return Env("SCHEDULER_URL", "http://localhost:8082") }
func CoordinatorURL() string { return Env("COORDINATOR_URL", "http://localhost:8083") }

// CoordinatorGRPCURL is where the gateway forwards agent sessions.
func CoordinatorGRPCURL() string { return Env("COORDINATOR_GRPC_URL", "http://localhost:50051") }

func InferenceGatewayURL() string { return Env("INFERENCE_GATEWAY_URL", "http://localhost:8085") }
func TrustEngineURL() string      { return Env("TRUST_ENGINE_URL", "http://localhost:8087") }
func WebURL() string              { return Env("WEB_URL", "http://localhost:3000") }

// PublicURL is the externally reachable base URL of the gateway. It appears in
// install commands and in the endpoint shown to customers.
//
// In development it may be left unset, and is then empty: the control API
// hands out the address each request arrived on, so the stack is usable from
// other machines on the network without configuration. Production never
// trusts the request for this (a forged Host header would otherwise end up in
// password-reset emails), so it falls back to a fixed default.
func PublicURL() string {
	if IsProduction() {
		return Env("PUBLIC_URL", "http://localhost:8080")
	}
	return Env("PUBLIC_URL", "")
}

// CoordinatorPublicURL is the address host agents dial. Unset, it is the same
// as the public URL: the gateway carries agent sessions on its own port, so
// one address serves the console, the API, the installers and the agents. Set
// it only when agents must use a different name, as behind a proxy that gives
// the coordinator its own hostname.
func CoordinatorPublicURL() string { return Env("COORDINATOR_PUBLIC_URL", "") }

// CoordinatorURLs is the full list of addresses agents may use, pushed to each
// agent when it connects so the platform can be moved without touching hosts.
func CoordinatorURLs() []string {
	var out []string
	for _, u := range strings.Split(Env("COORDINATOR_URLS", ""), ",") {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// HeartbeatTimeout is how long a host may be silent before it is offline.
// SRS FR-40: heartbeat every 5 s, 3 missed → drained.
func HeartbeatTimeout() time.Duration {
	return time.Duration(EnvInt("HOST_HEARTBEAT_TIMEOUT_SEC", 15)) * time.Second
}
