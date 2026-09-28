// Package main implements the AyeusANN Billing Meter: wallets, usage reports,
// invoices, top-ups and ledger reconciliation (Architecture §4, §9).
//
// Usage is recorded by the inference gateway through internal/billing in the
// same transaction as the wallet debit; this service reads and reconciles it.
package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/platform"
)

func main() {
	port := platform.EnvInt("BILLING_PORT", 8086)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "billing-meter")

	jwtSecret, err := platform.JWTSecret()
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

	rz := newRazorpay(os.Getenv("RAZORPAY_KEY_ID"), os.Getenv("RAZORPAY_KEY_SECRET"), os.Getenv("RAZORPAY_WEBHOOK_SECRET"))
	if rz == nil {
		logger.Warn("Razorpay not configured; online top-ups disabled")
	}

	meter := &Meter{
		db:              dbClient,
		ledger:          db.NewLedgerService(dbClient),
		log:             logger,
		razorpay:        rz,
		allowTestCredit: !platform.IsProduction(),
	}

	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			if err := meter.Reconcile(ctx); err != nil && ctx.Err() == nil {
				logger.Error("reconciliation failed", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	srv, err := platform.NewServer(platform.ServiceConfig{Name: "billing-meter", Version: "0.3.0", Port: port})
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}
	srv.Mux.Handle("/v1/", meter.Routes(tm, auth.NewPGRevocationStore(dbClient.Pool)))
	srv.SetReadyCheck(func(ctx context.Context) error { return dbClient.Pool.Ping(ctx) })
	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
