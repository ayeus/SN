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
}

// A machine that fails to start a model is not asked for that model again
// straight away: placing the replacement on the best-scoring machine would
// pick the same one, and it would fail the same way, every second, for ever.
// It rests for a minute, then two, doubling up to half an hour for as long as
// it keeps failing; one successful start clears it.
const (
	firstCooldown = time.Minute
	maxCooldown   = 30 * time.Minute
	// failureWindow is how far back start failures are counted.
	failureWindow = 6 * time.Hour
	// giveUpAfterHosts is how many different machines must fail to start a
	// deployment before it is marked failed. One bad machine is that
	// machine's problem; three in a row says the model or the request is.
	giveUpAfterHosts = 3
)

// cooldownAfter is how long a machine rests after n start failures in a row.
func cooldownAfter(n int) time.Duration {
	d := firstCooldown
	for ; n > 1 && d < maxCooldown; n-- {
		d *= 2
	}
	return min(d, maxCooldown)
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
	placingSince                                time.Time
}

// Tick runs one reconciliation pass.
func (r *Reconciler) Tick(ctx context.Context) error {
	var errs []error
	deps, err := r.loadDeployments(ctx)
	if err != nil {
		return err
	}

	var candidates []placement.Candidate
	var resting map[string]map[string]bool
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
				if resting, err = r.restingHosts(ctx, time.Now()); err != nil {
					return err
				}
				loaded = true
			}
			var placed []string
			placed, err = r.ensureReplicas(ctx, d, candidates, resting[d.modelID])
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
		       d.min_replicas, d.resident_in, m.is_byo, m.min_vram_gb, m.runtime_refs, d.placing_since
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
			&d.lastError, &d.minReplicas, &d.residentIN, &d.isBYO, &d.modelMinVram, &d.runtimeRefs, &d.placingSince); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
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

// restingHosts returns, for each model, the machines that failed to start it
// recently and are still inside their cool-down.
//
// It is read from the replicas themselves: a replica that failed without ever
// serving is a start failure on its host for its model. Failures are counted
// since that machine last started the model successfully, so one good start
// wipes the slate.
func (r *Reconciler) restingHosts(ctx context.Context, now time.Time) (map[string]map[string]bool, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT f.host_id, d.model_id, COUNT(*), MAX(f.stopped_at)
		FROM replicas f
		JOIN deployments d ON d.id = f.deployment_id
		WHERE f.state = 'failed' AND f.started_at IS NULL
		  AND f.stopped_at > NOW() - ($1 * INTERVAL '1 second')
		  AND f.stopped_at > COALESCE((
		        SELECT MAX(s.started_at) FROM replicas s
		        JOIN deployments sd ON sd.id = s.deployment_id
		        WHERE s.host_id = f.host_id AND sd.model_id = d.model_id AND s.started_at IS NOT NULL
		      ), 'epoch')
		GROUP BY f.host_id, d.model_id;
	`, int(failureWindow.Seconds()))
	if err != nil {
		return nil, fmt.Errorf("scheduler: reading start failures: %w", err)
	}
	defer rows.Close()
	out := map[string]map[string]bool{}
	for rows.Next() {
		var host, model string
		var n int
		var last time.Time
		if err := rows.Scan(&host, &model, &n, &last); err != nil {
			return nil, err
		}
		if now.Before(last.Add(cooldownAfter(n))) {
			if out[model] == nil {
				out[model] = map[string]bool{}
			}
			out[model][host] = true
		}
	}
	return out, rows.Err()
}

// giveUp marks a deployment failed once enough different machines have failed
// to start it, with the last machine's reason. It reports whether it did.
func (r *Reconciler) giveUp(ctx context.Context, d deployment) (bool, error) {
	var hosts int
	var lastError *string
	if err := r.db.Pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT host_id), (ARRAY_AGG(last_error ORDER BY stopped_at DESC))[1]
		FROM replicas
		WHERE deployment_id = $1 AND state = 'failed' AND started_at IS NULL
		  AND stopped_at > GREATEST($2::TIMESTAMPTZ, COALESCE(
		        (SELECT MAX(started_at) FROM replicas WHERE deployment_id = $1), 'epoch'));
	`, d.id, d.placingSince).Scan(&hosts, &lastError); err != nil {
		return false, err
	}
	if hosts < giveUpAfterHosts {
		return false, nil
	}
	msg := fmt.Sprintf("%d machines failed to start this model.", hosts)
	if lastError != nil && *lastError != "" {
		msg += " The last one reported: " + *lastError
	}
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE deployments SET state = 'failed', last_error = $2, state_changed_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND state <> 'failed';
	`, d.id, msg)
	if err != nil || tag.RowsAffected() == 0 {
		return err == nil, err
	}
	return true, lifecycle.Event(ctx, r.db.Pool, d.id, nil, "error", lifecycle.Failed, msg)
}

// ensureReplicas places replicas until the deployment has min_replicas active.
// It returns the GPU ids it reserved so the caller can drop them from the
// shared candidate list. resting holds the machines sitting out a cool-down
// for this deployment's model.
func (r *Reconciler) ensureReplicas(ctx context.Context, d deployment, candidates []placement.Candidate, resting map[string]bool) ([]string, error) {
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
	// Nothing is running and nothing is starting: if machine after machine
	// has failed to start it, stop trying and say why.
	if active == 0 {
		if gone, err := r.giveUp(ctx, d); err != nil || gone {
			return nil, err
		}
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
		RestingHosts:      resting,
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
			won, err := r.reserve(ctx, d, c, target)
			if errors.Is(err, errSatisfied) {
				return placed, nil
			}
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
// claim is conditional, so two reconcilers cannot reserve the same device, and
// the replica count is re-read under the deployment's row lock, so they cannot
// both fill the same slot either.
func (r *Reconciler) reserve(ctx context.Context, d deployment, c placement.Scored, target int) (bool, error) {
	won := false
	err := r.db.ExecTx(ctx, func(tx pgx.Tx) error {
		won = false
		var desired string
		if err := tx.QueryRow(ctx, `SELECT desired_state FROM deployments WHERE id = $1 FOR UPDATE;`, d.id).Scan(&desired); err != nil {
			return err
		}
		var active int
		var onHost bool
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*), COALESCE(BOOL_OR(host_id = $2), FALSE) FROM replicas
			WHERE deployment_id = $1 AND state IN ('pending', 'pulling', 'loading', 'warming', 'serving', 'degraded');
		`, d.id, c.HostID).Scan(&active, &onHost); err != nil {
			return err
		}
		if desired != lifecycle.DesiredRunning || active >= target {
			return errSatisfied
		}
		if onHost {
			return errLostRace
		}

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

var (
	errLostRace = errors.New("gpu already reserved")
	// errSatisfied means another reconciler filled the deployment, or it was
	// stopped, between the count and the reservation.
	errSatisfied = errors.New("deployment needs no more replicas")
)

func cacheNote(c placement.Scored) string {
	for _, m := range c.CachedModels {
		if m == c.RuntimeModel {
			return ", weights already cached"
		}
	}
	return ""
}

// noCapacity records why a deployment is waiting. Waiting is never turned
// into a failure: on a network of a few machines the GPU it needs is often
// simply switched off for the night, and a deployment that gave up at ten
// minutes would have to be restarted by hand every morning.
func (r *Reconciler) noCapacity(ctx context.Context, d deployment, msg string) error {
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
