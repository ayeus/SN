// Package main implements the AyeusANN Billing Meter.
// Responsibilities: usage event ingestion, wallet ledger debits, invoices, payouts.
package main

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/platform"
)

func main() {
	port, _ := strconv.Atoi(platform.MustEnv("BILLING_PORT", "8086"))
	dbURL := platform.MustEnv("DATABASE_URL", "postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann?sslmode=disable")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbClient, err := db.NewClient(ctx, db.Config{URL: dbURL})
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer dbClient.Close()

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
	meter.RegisterRoutes(srv.Mux)

	srv.Mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"AyeusANN-billing-meter","version":"0.2.0"}`))
	})

	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

