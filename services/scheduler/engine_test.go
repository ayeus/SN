package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/domain"
)

func getTestDBURL() string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann?sslmode=disable"
}

func TestSchedulerEnginePlacement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dbClient, err := db.NewClient(ctx, db.Config{URL: getTestDBURL()})
	if err != nil {
		t.Fatalf("Failed to connect to test database: %v", err)
	}
	defer dbClient.Close()

	// Clean existing test hosts for test isolation
	_, _ = dbClient.Pool.Exec(ctx, "DELETE FROM hosts WHERE name LIKE 'sched-host-%'")

	scheduler := NewSchedulerEngine(dbClient)

	// Seed an active host with 24GB GPU and 100 reputation
	hostName := "sched-host-" + uuid.New().String()[:8]
	var hostID, gpuID string

	err = dbClient.Pool.QueryRow(ctx, `
		INSERT INTO hosts (name, tier, region, reputation, status)
		VALUES ($1, 't2', 'IN-SOUTH', 100, 'active')
		RETURNING id;
	`, hostName).Scan(&hostID)
	if err != nil {
		t.Fatalf("Failed to seed host: %v", err)
	}

	gpuUUID := "GPU-sched-" + uuid.New().String()
	err = dbClient.Pool.QueryRow(ctx, `
		INSERT INTO gpus (host_id, model, vram_gb, uuid, status)
		VALUES ($1, 'NVIDIA RTX 4090', 24, $2, 'available')
		RETURNING id;
	`, hostID, gpuUUID).Scan(&gpuID)
	if err != nil {
		t.Fatalf("Failed to seed GPU: %v", err)
	}

	// 1. Schedule placement matching T2, IN-SOUTH, 16GB VRAM -> Should succeed and select hostID
	t.Run("SuccessfulPlacementMatchingConstraints", func(t *testing.T) {
		res, err := scheduler.SchedulePlacement(ctx, PlacementRequest{
			Tier:      domain.TierT2,
			Region:    domain.RegionInSouth,
			MinVramGB: 16,
		})

		if err != nil {
			t.Fatalf("SchedulePlacement failed: %v", err)
		}

		if res.HostID != hostID || res.GpuID != gpuID {
			t.Fatalf("Expected placement (host=%s, gpu=%s), got (host=%s, gpu=%s)", hostID, gpuID, res.HostID, res.GpuID)
		}
	})

	// 2. Schedule placement with 48GB VRAM constraint -> Should fail with ErrNoSuitableHostFound
	t.Run("UnsatisfiableVRAMConstraint", func(t *testing.T) {
		_, err := scheduler.SchedulePlacement(ctx, PlacementRequest{
			Tier:      domain.TierT2,
			Region:    domain.RegionInSouth,
			MinVramGB: 48,
		})

		if err != ErrNoSuitableHostFound {
			t.Fatalf("Expected ErrNoSuitableHostFound, got %v", err)
		}
	})
}
