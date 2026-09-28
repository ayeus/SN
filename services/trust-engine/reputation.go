package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Reputation weights, PRD F-12: uptime 40% · sampled re-execution correctness
// 25% · benchmark stability 15% · age 10% · incident rate 10%.
const (
	wUptime      = 0.40
	wCorrectness = 0.25
	wStability   = 0.15
	wAge         = 0.10
	wIncidents   = 0.10
)

// Lifecycle thresholds (UML §5).
const (
	demoteBelow        = 40 // ACTIVE → DEMOTED when reputation < threshold
	recoverAt          = 50 // DEMOTED → ACTIVE when the score recovers
	probationPassScore = 60 // PROBATION → ACTIVE: uptime + score ok
	ageFullCreditDays  = 30 // PRD F-12: T3→T2 review at 30 days
)

// Inputs are the measured facts a score is derived from. A nil component has
// no evidence yet and is left out of the weighting rather than guessed.
type Inputs struct {
	UptimePct       float64
	SuccessRatePct  *float64 // request success rate; stands in for re-execution until reference nodes exist
	StabilityPct    *float64 // 100 − coefficient of variation of compute benchmarks
	AgeDays         int
	Incidents30d    int
	IncidentRatePct float64
}

// Components explain a score: the "why was I demoted" view (SRS FR-63).
type Components struct {
	Uptime      float64  `json:"uptime"`
	Correctness *float64 `json:"correctness"`
	Stability   *float64 `json:"stability"`
	Age         float64  `json:"age"`
	Incidents   float64  `json:"incidents"`
}

// Score combines inputs into 0–100, renormalising over present evidence.
func Score(in Inputs) (int, Components) {
	c := Components{
		Uptime:    clamp(in.UptimePct),
		Age:       clamp(float64(in.AgeDays) / ageFullCreditDays * 100),
		Incidents: clamp(100 - float64(in.Incidents30d)*20),
	}
	sum := wUptime*c.Uptime + wAge*c.Age + wIncidents*c.Incidents
	weight := wUptime + wAge + wIncidents
	if in.SuccessRatePct != nil {
		v := clamp(*in.SuccessRatePct)
		c.Correctness = &v
		sum += wCorrectness * v
		weight += wCorrectness
	}
	if in.StabilityPct != nil {
		v := clamp(*in.StabilityPct)
		c.Stability = &v
		sum += wStability * v
		weight += wStability
	}
	return int(math.Round(sum / weight)), c
}

// Stability returns 100 − CV% over benchmark scores, or nil with fewer than two.
func Stability(scores []float64) *float64 {
	if len(scores) < 2 {
		return nil
	}
	var mean float64
	for _, s := range scores {
		mean += s
	}
	mean /= float64(len(scores))
	if mean <= 0 {
		return nil
	}
	var variance float64
	for _, s := range scores {
		variance += (s - mean) * (s - mean)
	}
	cv := math.Sqrt(variance/float64(len(scores))) / mean * 100
	v := clamp(100 - cv)
	return &v
}

func clamp(v float64) float64 { return math.Max(0, math.Min(100, v)) }

// ReputationEngine computes and stores reputation.
type ReputationEngine struct {
	db *db.Client
}

// Snapshot is a stored score with its explanation.
type Snapshot struct {
	domain.ReputationSnapshot
	Components Components `json:"components"`
	Status     string     `json:"host_status"`
}

// Compute measures a host, stores a snapshot, and applies lifecycle
// transitions driven by the score.
func (e *ReputationEngine) Compute(ctx context.Context, hostID string) (*Snapshot, error) {
	var created time.Time
	var status string
	var probationUntil *time.Time
	if err := e.db.Pool.QueryRow(ctx, `SELECT created_at, status, probation_until FROM hosts WHERE id = $1;`, hostID).
		Scan(&created, &status, &probationUntil); err != nil {
		return nil, fmt.Errorf("trust: host %s not found: %w", hostID, err)
	}

	// Uptime: minutes with at least one telemetry sample over the last 24 h, or
	// since enrolment if younger.
	windowStart := created
	if since := time.Now().Add(-24 * time.Hour); windowStart.Before(since) {
		windowStart = since
	}
	var activeMinutes int
	if err := e.db.Pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT DATE_TRUNC('minute', ts)) FROM host_telemetry WHERE host_id = $1 AND ts >= $2;
	`, hostID, windowStart).Scan(&activeMinutes); err != nil {
		return nil, err
	}
	windowMinutes := math.Max(1, time.Since(windowStart).Minutes())
	in := Inputs{
		UptimePct: float64(activeMinutes) / windowMinutes * 100,
		AgeDays:   int(time.Since(created).Hours() / 24),
	}

	var total, failed int64
	if err := e.db.Pool.QueryRow(ctx, `
		SELECT COUNT(*), COUNT(*) FILTER (WHERE status <> 'success')
		FROM usage_events WHERE host_id = $1 AND ts >= NOW() - INTERVAL '30 days';
	`, hostID).Scan(&total, &failed); err == nil && total > 0 {
		v := float64(total-failed) / float64(total) * 100
		in.SuccessRatePct = &v
	}

	rows, err := e.db.Pool.Query(ctx, `
		SELECT score_compute FROM benchmarks WHERE host_id = $1 AND score_compute IS NOT NULL ORDER BY ran_at DESC LIMIT 10;
	`, hostID)
	if err == nil {
		var scores []float64
		for rows.Next() {
			var s float64
			if rows.Scan(&s) == nil {
				scores = append(scores, s)
			}
		}
		rows.Close()
		in.StabilityPct = Stability(scores)
	}

	_ = e.db.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM trust_incidents WHERE host_id = $1 AND created_at >= NOW() - INTERVAL '30 days';
	`, hostID).Scan(&in.Incidents30d)
	in.IncidentRatePct = float64(in.Incidents30d)

	score, comps := Score(in)
	snap := &Snapshot{Components: comps}
	snap.HostID, snap.Score, snap.AgeDays, snap.ComputedAt = hostID, score, in.AgeDays, time.Now()
	snap.UptimePct = float32(comps.Uptime)
	if comps.Correctness != nil {
		v := float32(*comps.Correctness)
		snap.CorrectnessPct = &v
	}
	if comps.Stability != nil {
		v := float32(*comps.Stability)
		snap.BenchmarkStability = &v
	}
	ir := float32(in.IncidentRatePct)
	snap.IncidentRate = &ir

	next := nextStatus(status, score, probationUntil)
	snap.Status = next

	err = e.db.ExecTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO reputation_snapshots (host_id, score, uptime_pct, correctness_pct, benchmark_stability, age_days, incident_rate)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id;
		`, hostID, score, snap.UptimePct, snap.CorrectnessPct, snap.BenchmarkStability, snap.AgeDays, snap.IncidentRate).Scan(&snap.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE hosts SET reputation = $2, status = $3, updated_at = NOW() WHERE id = $1;`, hostID, score, next)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("trust: failed to store reputation: %w", err)
	}
	return snap, nil
}

// nextStatus applies the score-driven transitions of UML §5. Statuses owned by
// other components (offline, draining, banned, benchmarking) are untouched.
func nextStatus(status string, score int, probationUntil *time.Time) string {
	switch status {
	case domain.HostStatusProbation:
		if score < demoteBelow {
			return domain.HostStatusDemoted
		}
		if probationUntil != nil && time.Now().After(*probationUntil) && score >= probationPassScore {
			return domain.HostStatusActive
		}
	case domain.HostStatusActive:
		if score < demoteBelow {
			return domain.HostStatusDemoted
		}
	case domain.HostStatusDemoted:
		if score >= recoverAt {
			return domain.HostStatusActive
		}
	}
	return status
}

// RecordIncident logs an incident and recomputes the host.
func (e *ReputationEngine) RecordIncident(ctx context.Context, hostID, kind, severity string, evidence json.RawMessage) (*domain.TrustIncident, error) {
	if len(evidence) == 0 {
		evidence = json.RawMessage(`{}`)
	}
	var inc domain.TrustIncident
	err := e.db.Pool.QueryRow(ctx, `
		INSERT INTO trust_incidents (host_id, kind, severity, evidence_json)
		VALUES ($1, $2, $3, $4)
		RETURNING id, host_id, kind, severity, evidence_json, action, resolved, created_at;
	`, hostID, kind, severity, evidence).Scan(&inc.ID, &inc.HostID, &inc.Kind, &inc.Severity, &inc.EvidenceJSON, &inc.Action, &inc.Resolved, &inc.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("trust: failed to record incident: %w", err)
	}
	_, _ = e.Compute(ctx, hostID)
	return &inc, nil
}

// RecomputeAll refreshes every host that is not banned or decommissioned.
func (e *ReputationEngine) RecomputeAll(ctx context.Context) (int, error) {
	rows, err := e.db.Pool.Query(ctx, `SELECT id FROM hosts WHERE deleted_at IS NULL AND status <> 'banned';`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	n := 0
	for _, id := range ids {
		if _, err := e.Compute(ctx, id); err == nil {
			n++
		}
	}
	return n, nil
}
