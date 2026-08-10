package main

import (
	"context"
	"errors"
	"strings"

	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/domain"
)

var (
	ErrNoSuitableHostFound = errors.New("scheduler: no active host found matching tier, region, VRAM, and reputation constraints")
)

type PlacementRequest struct {
	DeploymentID string
	OrgID        string
	ModelID      string
	Tier         string
	Region       string
	MinVramGB    int
}

type PlacementResult struct {
	HostID    string
	GpuID     string
	OverlayIP string
	Tier      string
	Region    string
}

type SchedulerEngine struct {
	db *db.Client
}

func NewSchedulerEngine(database *db.Client) *SchedulerEngine {
	return &SchedulerEngine{db: database}
}

// SchedulePlacement evaluates deployment constraints and selects the optimal host and GPU device.
func (e *SchedulerEngine) SchedulePlacement(ctx context.Context, req PlacementRequest) (*PlacementResult, error) {
	if req.Region == "" {
		req.Region = domain.RegionInSouth
	}
	if req.Tier == "" {
		req.Tier = domain.TierT2
	}

	// Query candidate hosts and GPUs matching constraints
	// Constraints enforced:
	// 1. Host status MUST be 'active'
	// 2. Host tier MUST match requested tier
	// 3. Host region MUST match requested region
	// 4. Host reputation MUST be >= 40
	// 5. GPU VRAM MUST be >= MinVramGB
	query := `
		SELECT h.id, h.overlay_ip, h.tier, h.region, g.id
		FROM hosts h
		JOIN gpus g ON g.host_id = h.id
		WHERE h.status = 'active'
		  AND h.deleted_at IS NULL
		  AND LOWER(h.tier) = LOWER($1)
		  AND UPPER(h.region) = UPPER($2)
		  AND h.reputation >= 40
		  AND g.vram_gb >= $3
		ORDER BY h.reputation DESC, h.last_heartbeat_at DESC
		LIMIT 1;
	`

	var res PlacementResult
	var overlayIP *string

	err := e.db.Pool.QueryRow(ctx, query, req.Tier, req.Region, req.MinVramGB).Scan(
		&res.HostID, &overlayIP, &res.Tier, &res.Region, &res.GpuID,
	)

	if err != nil {
		// Fallback for T3 spot tier if strict region placement has no active host
		if strings.EqualFold(req.Tier, domain.TierT3) {
			fallbackQuery := `
				SELECT h.id, h.overlay_ip, h.tier, h.region, g.id
				FROM hosts h
				JOIN gpus g ON g.host_id = h.id
				WHERE h.status = 'active'
				  AND h.deleted_at IS NULL
				  AND h.reputation >= 30
				  AND g.vram_gb >= $1
				ORDER BY h.reputation DESC
				LIMIT 1;
			`
			err = e.db.Pool.QueryRow(ctx, fallbackQuery, req.MinVramGB).Scan(
				&res.HostID, &overlayIP, &res.Tier, &res.Region, &res.GpuID,
			)
		}
	}

	if err != nil {
		return nil, ErrNoSuitableHostFound
	}

	if overlayIP != nil {
		res.OverlayIP = *overlayIP
	}

	return &res, nil
}
