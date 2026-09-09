// Package main implements the AyeusANN Billing Meter.
// Responsibilities: usage event ingestion, wallet ledger debits, invoices, payouts.
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

const (
	devJWTSecret     = "dev-only-insecure-jwt-signing-key-0001"
	devServiceSecret = "dev-only-insecure-internal-service-key-0001"
)

func main() {
	port, _ := strconv.Atoi(platform.MustEnv("BILLING_PORT", "8086"))
	dbURL, err := platform.RequireEnv("DATABASE_URL", "postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann?sslmode=disable")
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	jwtSecret, err := platform.RequireSecret("JWT_SECRET", devJWTSecret, 32)
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	serviceSecret, err := platform.RequireSecret("INTERNAL_SERVICE_SECRET", devServiceSecret, 32)
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbClient, err := db.NewClient(ctx, db.Config{URL: dbURL})
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer dbClient.Close()

	tm, err := auth.NewTokenManager(jwtSecret, 15*time.Minute, 7*24*time.Hour)
	if err != nil {
		log.Fatalf("failed to create token manager: %v", err)
	}
	revStore := auth.NewPGRevocationStore(dbClient.Pool)

	svcAuth, err := auth.NewServiceAuthenticator(serviceSecret, auth.ServiceBillingMeter)
	if err != nil {
		log.Fatalf("failed to create service authenticator: %v", err)
	}

	meter := NewMeterService(dbClient)

	srv, err := platform.NewServer(platform.ServiceConfig{
		Name:    "billing-meter",
		Version: "0.2.0",
		Port:    port,
	})
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	// Register billing meter API routes
	meter.RegisterRoutes(srv.Mux, svcAuth, tm, revStore)

	srv.Mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"AyeusANN-billing-meter","version":"0.2.0"}`))
	})

	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

