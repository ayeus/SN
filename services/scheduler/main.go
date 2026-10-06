// Package main implements the AyeusANN Scheduler.
//
// Architecture §4: "placement + rebalance loops". The scheduler reads intent
// from Postgres (deployments.desired_state) instead of receiving HTTP calls, so
// a deployment is placed even if the scheduler was down when it was created,
// and a replica lost to a failed host is backfilled without anyone asking.
package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/ayeus/ayeusann/internal/platform"
)

func main() {
	port := platform.EnvInt("SCHEDULER_PORT", 8082)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "scheduler")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbClient, err := platform.ConnectDB(ctx)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer dbClient.Close()

	rec := &Reconciler{
		db:               dbClient,
		log:              logger,
		heartbeatTimeout: platform.HeartbeatTimeout(),
		scheduleTimeout:  time.Duration(platform.EnvInt("SCHEDULE_TIMEOUT_SEC", 600)) * time.Second,
	}
	interval := time.Duration(platform.EnvInt("SCHEDULER_INTERVAL_MS", 1000)) * time.Millisecond

	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := rec.Tick(ctx); err != nil && ctx.Err() == nil {
					logger.Error("reconcile tick failed", "err", err)
				}
			}
		}
	}()

	// Housekeeping. usage_events is partitioned by month and inserts fail once
	// the pre-created partitions run out, so the window is extended here; the
	// remaining statements bound tables that otherwise grow without limit.
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			for name, q := range map[string]string{
				"usage partitions":    `SELECT maintain_usage_events_partitions(3);`,
				"revoked tokens":      `DELETE FROM revoked_tokens WHERE expires_at < NOW();`,
				"auth tokens":         `DELETE FROM auth_tokens WHERE expires_at < NOW() - INTERVAL '7 days';`,
				"idempotency keys":    `DELETE FROM idempotency_keys WHERE created_at < NOW() - INTERVAL '24 hours';`,
				"host telemetry":      `DELETE FROM host_telemetry WHERE ts < NOW() - INTERVAL '30 days';`,
				"registration tokens": `DELETE FROM host_registration_tokens WHERE expires_at < NOW() - INTERVAL '30 days';`,
			} {
				if _, err := dbClient.Pool.Exec(ctx, q); err != nil && ctx.Err() == nil {
					logger.Error("housekeeping failed", "task", name, "err", err)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	srv, err := platform.NewServer(platform.ServiceConfig{Name: "scheduler", Version: platform.Version, Port: port})
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}
	srv.SetReadyCheck(func(ctx context.Context) error { return dbClient.Pool.Ping(ctx) })
	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
