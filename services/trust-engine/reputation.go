package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/domain"
)

type ReputationEngine struct {
	db *db.Client
}

func NewReputationEngine(database *db.Client) *ReputationEngine {
	return &ReputationEngine{db: database}
}

// ComputeReputation calculates and records a host's reputation score (0 - 100).
// Formula: Score = (UptimePct * 0.40) + (CorrectnessPct * 0.30) + (BenchmarkScore * 0.20) + (AgeDays * 0.10) - (UnresolvedIncidents * 15)
func (e *ReputationEngine) ComputeReputation(ctx context.Context, hostID string) (*domain.ReputationSnapshot, error) {
	var uptimePct, correctnessPct, benchScore float32 = 99.0, 100.0, 80.0
	var ageDays int = 30
	var unresolvedIncidents int

	// Count unresolved trust incidents
	countQuery := `SELECT COUNT(*) FROM trust_incidents WHERE host_id = $1 AND resolved = FALSE;`
	err := e.db.Pool.QueryRow(ctx, countQuery, hostID).Scan(&unresolvedIncidents)
	if err != nil && err != pgx.ErrNoRows {
		return nil, fmt.Errorf("trust: failed to query incidents: %w", err)
	}

	// Calculate weighted score
	rawScore := (uptimePct * 0.40) + (correctnessPct * 0.30) + (benchScore * 0.20) + float32(ageDays)*0.10 - float32(unresolvedIncidents*15)

	finalScore := int(rawScore)
	if finalScore > 100 {
		finalScore = 100
	}
	if finalScore < 0 {
		finalScore = 0
	}

	snapshot := &domain.ReputationSnapshot{
		HostID:             hostID,
		Score:              finalScore,
		UptimePct:          uptimePct,
		CorrectnessPct:     &correctnessPct,
		BenchmarkStability: &benchScore,
		AgeDays:            ageDays,
		ComputedAt:         time.Now(),
	}

	// Transactionally save snapshot and update host reputation in DB
	err = e.db.ExecTx(ctx, func(tx pgx.Tx) error {
		snapQuery := `
			INSERT INTO reputation_snapshots (host_id, score, uptime_pct, correctness_pct, benchmark_stability, age_days)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id;
		`
		err := tx.QueryRow(ctx, snapQuery, hostID, finalScore, uptimePct, correctnessPct, benchScore, ageDays).Scan(&snapshot.ID)
		if err != nil {
			return fmt.Errorf("trust: failed to insert reputation snapshot: %w", err)
		}

		hostUpdateQuery := `UPDATE hosts SET reputation = $1, updated_at = NOW() WHERE id = $2;`
		_, err = tx.Exec(ctx, hostUpdateQuery, finalScore, hostID)
		if err != nil {
			return fmt.Errorf("trust: failed to update host reputation: %w", err)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return snapshot, nil
}

// RecordIncident logs a new trust incident (e.g. benchmark drift, spoofing, downtime) and reduces host score.
func (e *ReputationEngine) RecordIncident(ctx context.Context, hostID string, kind string, severity string, evidence string) (*domain.TrustIncident, error) {
	var incident domain.TrustIncident

	query := `
		INSERT INTO trust_incidents (host_id, kind, severity, evidence_json)
		VALUES ($1, $2, $3, $4::jsonb)
		RETURNING id, host_id, kind, severity, evidence_json, action, resolved, created_at;
	`
	err := e.db.Pool.QueryRow(ctx, query, hostID, kind, severity, fmt.Sprintf(`{"evidence":%q}`, evidence)).Scan(
		&incident.ID, &incident.HostID, &incident.Kind, &incident.Severity, &incident.EvidenceJSON, &incident.Action, &incident.Resolved, &incident.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("trust: failed to record incident: %w", err)
	}

	// Recompute host reputation after incident
	_, _ = e.ComputeReputation(ctx, hostID)

	return &incident, nil
}
