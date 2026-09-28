package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/lifecycle"
	"github.com/ayeus/ayeusann/internal/placement"
	"github.com/jackc/pgx/v5"
)

// Reconciler converges every deployment toward its desired state. It is the
// only component that creates replicas or decides to stop them; the
// coordinator executes those decisions against hosts.
type Reconciler struct {
	db               *db.Client
	log              *slog.Logger
	heartbeatTimeout time.Duration
	scheduleTimeout  time.Duration
}

// deployment is the subset of a deployment row the reconciler needs.
type deployment struct {
	id, orgID, modelID, modelName, tier, region string
	desired, state                              string
	lastError                                   *string
	minReplicas                                 int
	residentIN, isBYO                           bool
	modelMinVram                                int
	runtimeRefs                                 []byte
	stateChangedAt                              time.Time
}

// Tick runs one reconciliation pass.
func (r *Reconciler) Tick(ctx context.Context) error {
	var errs []error
	if err := r.pauseExhaustedWallets(ctx); err != nil {
		errs = append(errs, fmt.Errorf("spend cap: %w", err))
	}
	deps, err := r.loadDeployments(ctx)
	if err != nil {
		return err
	}

	var candidates []placement.Candidate
	loaded := false

	for _, d := range deps {
		switch d.desired {
		case lifecycle.DesiredStopped, lifecycle.DesiredPaused:
			if err := r.drain(ctx, d); err != nil {
				errs = append(errs, fmt.Errorf("drain %s: %w", d.id, err))
			}
		default:
			if !loaded {
				candidates, err = placement.LoadCandidates(ctx, r.db.Pool, r.heartbeatTimeout)
				if err != nil {
					return err
				}
				loaded = true
			}
			var placed []string
			placed, err = r.ensureReplicas(ctx, d, candidates)
			if err != nil {
				errs = append(errs, fmt.Errorf("place %s: %w", d.id, err))
			}
			candidates = without(candidates, placed)
		}
		if err := r.recompute(ctx, d.id); err != nil {
			errs = append(errs, fmt.Errorf("recompute %s: %w", d.id, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Reconciler) loadDeployments(ctx context.Context) ([]deployment, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT d.id, d.org_id, d.model_id, m.name, d.tier, d.region, d.desired_state, d.state, d.last_error,
		       d.min_replicas, d.resident_in, m.is_byo, m.min_vram_gb, m.runtime_refs, d.state_changed_at
		FROM deployments d
		JOIN models m ON m.id = d.model_id
		WHERE d.deleted_at IS NULL
		  AND NOT (d.desired_state = 'stopped' AND d.state = 'stopped')
		  AND NOT (d.desired_state = 'paused' AND d.state = 'paused')
		ORDER BY d.created_at;
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []deployment
	for rows.Next() {
		var d deployment
		if err := rows.Scan(&d.id, &d.orgID, &d.modelID, &d.modelName, &d.tier, &d.region, &d.desired, &d.state,
			&d.lastError, &d.minReplicas, &d.residentIN, &d.isBYO, &d.modelMinVram, &d.runtimeRefs, &d.stateChangedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// pauseExhaustedWallets enforces the hard spend cap (SRS FR-72, UML §4
// SERVING → PAUSED): a running deployment whose organisation has no spendable
// balance is paused until the customer tops up and resumes it.
func (r *Reconciler) pauseExhaustedWallets(ctx context.Context) error {
	rows, err := r.db.Pool.Query(ctx, `
		UPDATE deployments d
		SET desired_state = 'paused', updated_at = NOW(),
		    last_error = 'Paused automatically: wallet balance exhausted. Top up and resume to continue.'
		FROM (
		    SELECT o.id AS org_id
		    FROM organizations o
		    JOIN LATERAL (
		        SELECT balance_after FROM wallet_ledger WHERE org_id = o.id ORDER BY seq DESC LIMIT 1
		    ) b ON TRUE
		    LEFT JOIN wallet_settings ws ON ws.org_id = o.id
		    WHERE b.balance_after + COALESCE(ws.credit_limit, 0) <= 0
		) broke
		WHERE d.org_id = broke.org_id AND d.desired_state = 'running' AND d.deleted_at IS NULL
		RETURNING d.id;
	`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		r.log.Info("deployment paused by spend cap", "deployment_id", id)
		_ = lifecycle.Event(ctx, r.db.Pool, id, nil, "error", lifecycle.Paused,
			"Paused automatically: wallet balance exhausted. Top up and resume to continue.")
	}
	return rows.Err()
}

// drain moves a paused or stopped deployment's replicas toward STOPPED. A
// replica not yet dispatched has nothing running and stops immediately.
func (r *Reconciler) drain(ctx context.Context, d deployment) error {
	return r.db.ExecTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, state, dispatched_at IS NOT NULL FROM replicas
			WHERE deployment_id = $1 AND state IN ('pending', 'pulling', 'loading', 'warming', 'serving', 'degraded');
		`, d.id)
		if err != nil {
			return err
		}
		type rep struct {
			id, state  string
			dispatched bool
		}
		var reps []rep
		for rows.Next() {
			var x rep
			if err := rows.Scan(&x.id, &x.state, &x.dispatched); err != nil {
				rows.Close()
				return err
			}
			reps = append(reps, x)
		}
		rows.Close()
		for _, x := range reps {
			next, detail := lifecycle.Stopping, "stop requested"
			if x.state == lifecycle.Pending && !x.dispatched {
				next, detail = lifecycle.Stopped, "stopped before dispatch"
			}
			if _, _, err := lifecycle.SetReplicaState(ctx, tx, x.id, next, detail, ""); err != nil {
				return err
			}
		}
		return nil
	})
}

// ensureReplicas places replicas until the deployment has min_replicas active.
// It returns the GPU ids it reserved so the caller can drop them from the
// shared candidate list.
func (r *Reconciler) ensureReplicas(ctx context.Context, d deployment, candidates []placement.Candidate) ([]string, error) {
	if d.state == lifecycle.Failed {
		return nil, nil // sticky until the customer retries (UML §4 FAILED → PENDING)
	}

	var active int
	exclude := map[string]bool{}
	rows, err := r.db.Pool.Query(ctx, `
		SELECT host_id FROM replicas
		WHERE deployment_id = $1 AND state IN ('pending', 'pulling', 'loading', 'warming', 'serving', 'degraded');
	`, d.id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err == nil {
			exclude[h] = true
			active++
		}
	}
	rows.Close()

	target := d.minReplicas
	if target < 1 {
		target = 1
	}
	need := target - active
	if need <= 0 {
		return nil, nil
	}

	refs, err := placement.ParseRuntimeRefs(d.runtimeRefs)
	if err != nil {
		return nil, err
	}
	req := placement.Requirements{
		Tier:              d.tier,
		Region:            d.region,
		ResidentIN:        d.residentIN,
		IsBYO:             d.isBYO,
		RuntimeRefs:       refs,
		FallbackMinVramGB: d.modelMinVram,
		ExcludeHosts:      exclude,
	}

	var placed []string
	for need > 0 {
		ranked, rejections := placement.Rank(candidates, req)
		if len(ranked) == 0 {
			if active == 0 && len(placed) == 0 {
				return placed, r.noCapacity(ctx, d, placement.NoCapacityMessage(req, rejections, len(candidates)))
			}
			return placed, nil // partially placed; the next tick retries
		}
		ok := false
		for _, c := range ranked {
			won, err := r.reserve(ctx, d, c)
			if err != nil {
				return placed, err
			}
			if won {
				placed = append(placed, c.GPUID)
				req.ExcludeHosts[c.HostID] = true
				candidates = without(candidates, []string{c.GPUID})
				ok = true
				break
			}
		}
		if !ok {
			return placed, nil
		}
		need--
	}
	return placed, nil
}

// reserve inserts a replica and claims the GPU in one transaction. The GPU
// claim is conditional, so two reconcilers cannot reserve the same device.
func (r *Reconciler) reserve(ctx context.Context, d deployment, c placement.Scored) (bool, error) {
	won := false
	err := r.db.ExecTx(ctx, func(tx pgx.Tx) error {
		var replicaID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO replicas (deployment_id, host_id, gpu_id, state, overlay_ip, healthy, detail)
			SELECT $1, $2, $3, 'pending', h.overlay_ip, FALSE, 'placed; waiting for dispatch'
			FROM hosts h WHERE h.id = $2
			RETURNING id;
		`, d.id, c.HostID, c.GPUID).Scan(&replicaID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE gpus SET status = 'reserved', replica_id = $2
			WHERE id = $1 AND status = 'available' AND replica_id IS NULL;
		`, c.GPUID, replicaID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errLostRace
		}
		if _, err := tx.Exec(ctx, `UPDATE deployments SET last_error = NULL WHERE id = $1;`, d.id); err != nil {
			return err
		}
		// Customers never learn which host serves them (Architecture §5); the
		// event names the tier, region and GPU class only.
		msg := fmt.Sprintf("Placed on a %s host in %s (%s, score %.2f%s)",
			strings.ToUpper(c.HostTier), c.Region, c.GPUModel, c.Score, cacheNote(c))
		rid := replicaID
		if err := lifecycle.Event(ctx, tx, d.id, &rid, "info", lifecycle.Scheduling, msg); err != nil {
			return err
		}
		won = true
		return nil
	})
	if errors.Is(err, errLostRace) {
		return false, nil
	}
	if err == nil {
		r.log.Info("replica placed", "deployment_id", d.id, "host_id", c.HostID, "gpu_id", c.GPUID, "score", c.Score)
	}
	return won, err
}

var errLostRace = errors.New("gpu already reserved")

func cacheNote(c placement.Scored) string {
	for _, m := range c.CachedModels {
		if m == c.RuntimeModel {
			return ", weights already cached"
		}
	}
	return ""
}

// noCapacity records why a deployment is waiting, and fails it once it has
// waited longer than the scheduling timeout (UML §4: SCHEDULING → FAILED).
func (r *Reconciler) noCapacity(ctx context.Context, d deployment, msg string) error {
	if d.state == lifecycle.Pending && time.Since(d.stateChangedAt) > r.scheduleTimeout {
		failMsg := fmt.Sprintf("No capacity after %s. %s", r.scheduleTimeout, msg)
		if _, err := r.db.Pool.Exec(ctx, `
			UPDATE deployments SET state = 'failed', last_error = $2, state_changed_at = NOW(), updated_at = NOW()
			WHERE id = $1;
		`, d.id, failMsg); err != nil {
			return err
		}
		return lifecycle.Event(ctx, r.db.Pool, d.id, nil, "error", lifecycle.Failed, failMsg)
	}
	if d.lastError != nil && *d.lastError == msg {
		return nil
	}
	if _, err := r.db.Pool.Exec(ctx, `UPDATE deployments SET last_error = $2 WHERE id = $1;`, d.id, msg); err != nil {
		return err
	}
	return lifecycle.Event(ctx, r.db.Pool, d.id, nil, "info", "", msg)
}

func (r *Reconciler) recompute(ctx context.Context, id string) error {
	return r.db.ExecTx(ctx, func(tx pgx.Tx) error {
		_, _, err := lifecycle.Recompute(ctx, tx, id)
		return err
	})
}

func without(cands []placement.Candidate, gpuIDs []string) []placement.Candidate {
	if len(gpuIDs) == 0 {
		return cands
	}
	drop := map[string]bool{}
	for _, id := range gpuIDs {
		drop[id] = true
	}
	out := cands[:0:0]
	for _, c := range cands {
		if !drop[c.GPUID] {
			out = append(out, c)
		}
	}
	return out
}
