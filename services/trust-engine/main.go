// Package main implements the AyeusANN Trust Engine: reputation scoring,
// incidents, and score-driven host lifecycle (Architecture §4, PRD F-12).
package main

import (
	"context"
	"encoding/json"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/httpx"
	"github.com/ayeus/ayeusann/internal/platform"
)

var incidentKinds = map[string]bool{"mismatch": true, "spoof": true, "abuse": true, "downtime": true, "benchmark_drift": true}
var severities = map[string]bool{"low": true, "medium": true, "high": true, "critical": true}

func main() {
	port := platform.EnvInt("TRUST_PORT", 8087)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "trust-engine")

	jwtSecret, err := platform.JWTSecret()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}
	internalSecret, err := platform.InternalSecret()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dbClient, err := platform.ConnectDB(ctx)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer dbClient.Close()

	tm, err := auth.NewTokenManager(jwtSecret, 15*time.Minute, 7*24*time.Hour)
	if err != nil {
		log.Fatalf("failed to create token manager: %v", err)
	}
	svcAuth, err := auth.NewServiceAuthenticator(internalSecret, auth.ServiceTrustEngine)
	if err != nil {
		log.Fatalf("failed to create service authenticator: %v", err)
	}
	engine := &ReputationEngine{db: dbClient}

	// SRS FR-61: recomputed at least daily. Hourly keeps new hosts' scores and
	// probation exits timely at negligible cost.
	interval := time.Duration(platform.EnvInt("REPUTATION_INTERVAL_SEC", 3600)) * time.Second
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			if n, err := engine.RecomputeAll(ctx); err != nil && ctx.Err() == nil {
				logger.Error("reputation recompute failed", "err", err)
			} else {
				logger.Info("reputation recomputed", "hosts", n)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	srv, err := platform.NewServer(platform.ServiceConfig{Name: "trust-engine", Version: "0.3.0", Port: port})
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	authn := auth.NewMiddleware(tm, auth.NewPGRevocationStore(dbClient.Pool))
	internal := svcAuth.RequireInternalService(auth.ServiceCoordinator, auth.ServiceInferenceGateway, auth.ServiceControlAPI)

	mux := http.NewServeMux()

	// A host owner reads their own host's reputation and its inputs (FR-63).
	mux.Handle("GET /v1/reputation/{host_id}", authn.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := auth.GetClaims(r.Context())
		var owner string
		if err := dbClient.Pool.QueryRow(r.Context(), `SELECT user_id::TEXT FROM hosts WHERE id::TEXT = $1 AND deleted_at IS NULL;`,
			r.PathValue("host_id")).Scan(&owner); err != nil || owner != c.UserID {
			httpx.WriteProblem(w, http.StatusNotFound, "Host not found")
			return
		}
		snap, err := engine.Compute(r.Context(), r.PathValue("host_id"))
		if err != nil {
			httpx.WriteProblem(w, http.StatusInternalServerError, "Failed to compute reputation")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, snap)
	})))

	// Internal: incidents are raised by platform components, never by the
	// public — previously anyone could POST an incident and tank any host.
	mux.Handle("POST /internal/v1/incidents", internal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			HostID   string          `json:"host_id"`
			Kind     string          `json:"kind"`
			Severity string          `json:"severity"`
			Evidence json.RawMessage `json:"evidence"`
		}
		if httpx.DecodeJSON(w, r, &req) != nil {
			return
		}
		req.Kind, req.Severity = strings.ToLower(req.Kind), strings.ToLower(req.Severity)
		if req.Severity == "" {
			req.Severity = "medium"
		}
		if req.HostID == "" || !incidentKinds[req.Kind] || !severities[req.Severity] {
			httpx.WriteProblem(w, http.StatusBadRequest, "host_id, a known kind and a known severity are required")
			return
		}
		inc, err := engine.RecordIncident(r.Context(), req.HostID, req.Kind, req.Severity, req.Evidence)
		if err != nil {
			httpx.WriteProblem(w, http.StatusInternalServerError, "Failed to record incident")
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, inc)
	})))

	mux.Handle("POST /internal/v1/reputation/{host_id}/compute", internal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snap, err := engine.Compute(r.Context(), r.PathValue("host_id"))
		if err != nil {
			httpx.WriteProblem(w, http.StatusNotFound, "Host not found")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, snap)
	})))

	srv.Mux.Handle("/v1/", platform.MetricsMiddleware("trust-engine", mux))
	srv.Mux.Handle("/internal/", mux)
	srv.SetReadyCheck(func(ctx context.Context) error { return dbClient.Pool.Ping(ctx) })
	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
