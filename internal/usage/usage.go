// Package usage records what each inference request consumed. One row per
// request is the source for dashboards, host activity and the trust engine's
// success rates.
package usage

import (
	"context"
	"fmt"

	"github.com/ayeus/ayeusann/internal/db"
	"github.com/jackc/pgx/v5"
)

// Statuses mirror the usage_events.status check constraint.
const (
	StatusSuccess   = "success"
	StatusError     = "error"
	StatusTimeout   = "timeout"
	StatusCancelled = "cancelled"
)

// Event is one completed (or failed) inference request.
type Event struct {
	RequestID    string
	OrgID        string
	DeploymentID string
	ReplicaID    string
	HostID       string
	ModelID      string
	Tier         string
	InputTokens  int
	OutputTokens int
	// GPUSeconds is wall-clock time the request held the replica's GPU. With one
	// job per GPU (ADR-008) this is the honest measure of occupancy.
	GPUSeconds float64
	DurationMs int
	Status     string
}

// Record writes the usage event and the replica's request counters in one
// transaction. It reports whether the request had already been recorded, so a
// retried write never counts twice.
//
// Failed requests are recorded too: the trust engine derives each host's
// success rate from them.
func Record(ctx context.Context, client *db.Client, e Event) (duplicate bool, err error) {
	if e.RequestID == "" || e.OrgID == "" || e.DeploymentID == "" || e.ReplicaID == "" || e.HostID == "" {
		return false, fmt.Errorf("usage: event is missing required identifiers")
	}
	if e.Status == "" {
		e.Status = StatusSuccess
	}

	err = client.ExecTx(ctx, func(tx pgx.Tx) error {
		duplicate = false
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM usage_events WHERE request_id = $1);`, e.RequestID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("usage: duplicate check failed: %w", err)
		}
		if exists {
			duplicate = true
			return nil
		}

		// ON CONFLICT without a target: usage_events is partitioned, and request_id
		// uniqueness is enforced by per-partition unique indexes, which a named
		// conflict target on the parent cannot reference. The amount columns
		// belong to billing, which is not built yet; they stay zero.
		tag, err := tx.Exec(ctx, `
			INSERT INTO usage_events (
				request_id, deployment_id, replica_id, host_id, org_id, model_id,
				input_tokens, output_tokens, gpu_seconds, tier,
				amount_customer, amount_host, duration_ms, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 0, 0, $11, $12)
			ON CONFLICT DO NOTHING;
		`, e.RequestID, e.DeploymentID, e.ReplicaID, e.HostID, e.OrgID, e.ModelID,
			e.InputTokens, e.OutputTokens, e.GPUSeconds, e.Tier, e.DurationMs, e.Status)
		if err != nil {
			return fmt.Errorf("usage: failed to insert event: %w", err)
		}
		if tag.RowsAffected() == 0 {
			duplicate = true
			return nil
		}

		failed := 0
		if e.Status != StatusSuccess {
			failed = 1
		}
		if _, err := tx.Exec(ctx, `
			UPDATE replicas
			SET total_requests = total_requests + 1,
			    failed_requests = failed_requests + $2
			WHERE id = $1;
		`, e.ReplicaID, failed); err != nil {
			return fmt.Errorf("usage: failed to update replica counters: %w", err)
		}
		return nil
	})
	return duplicate, err
}
