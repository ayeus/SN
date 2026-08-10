package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ayeus/ayeusann/internal/db"
)

func getTestDBURL() string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann?sslmode=disable"
}

func TestReputationEngineAndIncidents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dbClient, err := db.NewClient(ctx, db.Config{URL: getTestDBURL()})
	if err != nil {
		t.Fatalf("Failed to connect to test database: %v", err)
	}
	defer dbClient.Close()

	engine := NewReputationEngine(dbClient)

	// Create test host
	hostName := "trust-host-" + uuid.New().String()[:8]
	var hostID string
	err = dbClient.Pool.QueryRow(ctx, `
		INSERT INTO hosts (name, tier, region, reputation, status)
		VALUES ($1, 't2', 'IN-SOUTH', 50, 'active')
		RETURNING id;
	`, hostName).Scan(&hostID)

	if err != nil {
		t.Fatalf("Failed to insert test host: %v", err)
	}

	// 1. Initial Reputation Computation
	t.Run("ComputeInitialReputation", func(t *testing.T) {
		snap, err := engine.ComputeReputation(ctx, hostID)
		if err != nil {
			t.Fatalf("ComputeReputation error: %v", err)
		}
		if snap.Score <= 0 {
			t.Fatalf("Expected positive score, got %d", snap.Score)
		}

		// Verify host record updated
		var rep int
		_ = dbClient.Pool.QueryRow(ctx, `SELECT reputation FROM hosts WHERE id = $1;`, hostID).Scan(&rep)
		if rep != snap.Score {
			t.Fatalf("Expected host reputation %d, got %d", snap.Score, rep)
		}
	})

	// 2. Record Trust Incident & Verify Penalty
	t.Run("RecordIncidentAndPenalty", func(t *testing.T) {
		inc, err := engine.RecordIncident(ctx, hostID, "benchmark_drift", "high", "VRAM bandwidth dropped by 50%")
		if err != nil {
			t.Fatalf("RecordIncident error: %v", err)
		}
		if inc.ID == "" {
			t.Fatal("Expected non-empty incident ID")
		}

		// Recompute reputation and verify penalty deducted
		snapAfter, err := engine.ComputeReputation(ctx, hostID)
		if err != nil {
			t.Fatalf("ComputeReputation error: %v", err)
		}

		var newRep int
		_ = dbClient.Pool.QueryRow(ctx, `SELECT reputation FROM hosts WHERE id = $1;`, hostID).Scan(&newRep)
		if newRep >= 90 {
			t.Fatalf("Expected penalty deduction on incident, got reputation %d", newRep)
		}

		_ = snapAfter
	})
}
