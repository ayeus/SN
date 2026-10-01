// Package lifecycle owns the deployment and replica state machines (UML §4).
//
// Replica state is observed: agents report it through the coordinator, and the
// coordinator's sweeper marks replicas on vanished hosts failed. Deployment
// state is derived: Rollup computes it from the replicas and the customer's
// desired state. Both the coordinator (on every stage event) and the scheduler
// (on every loop) call Recompute, which is idempotent.
package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/ayeus/ayeusann/internal/db"
	"github.com/jackc/pgx/v5"
)

// Replica and deployment states, mirroring the schema check constraints.
const (
	Pending    = "pending"
	Scheduling = "scheduling"
	Pulling    = "pulling"
	Loading    = "loading"
	Warming    = "warming"
	Serving    = "serving"
	Degraded   = "degraded"
	Paused     = "paused"
	Stopping   = "stopping"
	Stopped    = "stopped"
	Failed     = "failed"
)

// Desired states set by the customer.
const (
	DesiredRunning = "running"
	DesiredPaused  = "paused"
	DesiredStopped = "stopped"
)

// ActiveReplicaStates are replica states that hold a GPU.
var ActiveReplicaStates = []string{Pending, Pulling, Loading, Warming, Serving, Degraded, Stopping}

// IsTerminalReplica reports whether a replica state releases its GPU.
func IsTerminalReplica(s string) bool { return s == Stopped || s == Failed }

// Counts summarises a deployment's replicas by state.
type Counts map[string]int

// Active returns the number of replicas still holding a GPU.
func (c Counts) Active() int {
	n := 0
	for _, s := range ActiveReplicaStates {
		n += c[s]
	}
	return n
}

// Rollup derives a deployment's observed state. current is the stored state,
// used to keep FAILED sticky and to report DEGRADED after a serving
// deployment loses its replicas (UML §4: SERVING → DEGRADED).
func Rollup(desired, current string, minReplicas int, c Counts) string {
	if minReplicas < 1 {
		minReplicas = 1
	}

	switch desired {
	case DesiredStopped, DesiredPaused:
		if c.Active() > 0 {
			return Stopping
		}
		if desired == DesiredPaused {
			return Paused
		}
		return Stopped
	}

	serving := c[Serving]
	switch {
	case serving >= minReplicas:
		return Serving
	case serving > 0:
		return Degraded
	}

	wasServing := current == Serving || current == Degraded
	if c.Active() > 0 {
		if wasServing {
			return Degraded
		}
		switch {
		case c[Warming] > 0:
			return Warming
		case c[Loading] > 0:
			return Loading
		case c[Pulling] > 0:
			return Pulling
		default:
			return Scheduling
		}
	}

	switch {
	case current == Failed:
		return Failed
	case wasServing:
		return Degraded
	default:
		return Pending
	}
}

// Event appends to the deployment event log.
func Event(ctx context.Context, q db.Querier, deploymentID string, replicaID *string, kind, state, message string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO deployment_events (deployment_id, replica_id, kind, state, message)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5);
	`, deploymentID, replicaID, kind, state, message)
	if err != nil {
		return fmt.Errorf("lifecycle: failed to record event: %w", err)
	}
	return nil
}

// ReleaseGPU frees the GPU a replica reserved (ADR-008: one replica per GPU).
func ReleaseGPU(ctx context.Context, q db.Querier, replicaID string) error {
	_, err := q.Exec(ctx, `
		UPDATE gpus SET status = 'available', replica_id = NULL WHERE replica_id = $1;
	`, replicaID)
	if err != nil {
		return fmt.Errorf("lifecycle: failed to release GPU: %w", err)
	}
	return nil
}

// SetReplicaState applies an observed replica state, logs it, and releases the
// GPU on a terminal state. It returns the replica's deployment so the caller
// can recompute it. A replica that is already terminal is left alone, so a late
// agent message cannot resurrect a stopped replica.
func SetReplicaState(ctx context.Context, q db.Querier, replicaID, state, detail, errMsg string) (string, bool, error) {
	// Lock order is always deployment, then replica. Logging an event takes a
	// shared lock on the deployment row (foreign key), and Recompute then needs
	// it exclusively; two replicas of one deployment reporting at once would
	// each hold the shared lock and deadlock on the upgrade. Taking the
	// exclusive lock first serialises them instead.
	var depID, prev string
	err := q.QueryRow(ctx, `SELECT deployment_id FROM replicas WHERE id = $1;`, replicaID).Scan(&depID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("lifecycle: failed to read replica: %w", err)
	}
	if _, err := q.Exec(ctx, `SELECT 1 FROM deployments WHERE id = $1 FOR UPDATE;`, depID); err != nil {
		return "", false, fmt.Errorf("lifecycle: failed to lock deployment: %w", err)
	}
	if err := q.QueryRow(ctx, `SELECT state FROM replicas WHERE id = $1 FOR UPDATE;`, replicaID).Scan(&prev); err != nil {
		return "", false, fmt.Errorf("lifecycle: failed to lock replica: %w", err)
	}
	if IsTerminalReplica(prev) {
		return depID, false, nil
	}
	// A replica being stopped only moves to a terminal state.
	if prev == Stopping && !IsTerminalReplica(state) {
		return depID, false, nil
	}

	_, err = q.Exec(ctx, `
		UPDATE replicas
		SET state = $2,
		    healthy = ($2 = 'serving'),
		    detail = NULLIF($3, ''),
		    last_error = COALESCE(NULLIF($4, ''), last_error),
		    started_at = CASE WHEN $2 = 'serving' AND started_at IS NULL THEN NOW() ELSE started_at END,
		    stopped_at = CASE WHEN $2 IN ('stopped', 'failed') THEN NOW() ELSE stopped_at END,
		    updated_at = NOW()
		WHERE id = $1;
	`, replicaID, state, detail, errMsg)
	if err != nil {
		return "", false, fmt.Errorf("lifecycle: failed to update replica: %w", err)
	}

	if IsTerminalReplica(state) {
		if err := ReleaseGPU(ctx, q, replicaID); err != nil {
			return "", false, err
		}
	}

	if prev != state {
		kind, msg := "replica", fmt.Sprintf("Replica %s: %s", short(replicaID), state)
		if detail != "" {
			msg += " — " + detail
		}
		if errMsg != "" {
			kind, msg = "error", fmt.Sprintf("Replica %s failed: %s", short(replicaID), errMsg)
		}
		rid := replicaID
		if err := Event(ctx, q, depID, &rid, kind, state, msg); err != nil {
			return "", false, err
		}
	}
	return depID, prev != state, nil
}

// Recompute derives and stores a deployment's state inside tx. It takes a row
// lock on the deployment so the coordinator and scheduler serialize.
func Recompute(ctx context.Context, tx pgx.Tx, deploymentID string) (string, bool, error) {
	var desired, current string
	var minReplicas int
	err := tx.QueryRow(ctx, `
		SELECT desired_state, state, min_replicas FROM deployments WHERE id = $1 FOR UPDATE;
	`, deploymentID).Scan(&desired, &current, &minReplicas)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("lifecycle: failed to read deployment: %w", err)
	}

	rows, err := tx.Query(ctx, `SELECT state, COUNT(*) FROM replicas WHERE deployment_id = $1 GROUP BY state;`, deploymentID)
	if err != nil {
		return "", false, fmt.Errorf("lifecycle: failed to count replicas: %w", err)
	}
	counts := Counts{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			rows.Close()
			return "", false, err
		}
		counts[s] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", false, err
	}

	next := Rollup(desired, current, minReplicas, counts)
	if next == current {
		return current, false, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE deployments
		SET state = $2, state_changed_at = NOW(), updated_at = NOW(),
		    last_error = CASE WHEN $2 = 'serving' THEN NULL ELSE last_error END
		WHERE id = $1;
	`, deploymentID, next); err != nil {
		return "", false, fmt.Errorf("lifecycle: failed to update deployment: %w", err)
	}
	if err := Event(ctx, tx, deploymentID, nil, "state", next, describe(current, next)); err != nil {
		return "", false, err
	}
	return next, true, nil
}

func describe(from, to string) string {
	switch to {
	case Scheduling:
		return "Capacity reserved; sending the job to the host"
	case Pulling:
		return "Host is downloading model weights"
	case Loading:
		return "Loading weights into GPU memory"
	case Warming:
		return "Warming up the model"
	case Serving:
		return "Serving — endpoint is live"
	case Degraded:
		return "Degraded — fewer replicas than requested; backfilling"
	case Stopping:
		return "Stopping replicas"
	case Stopped:
		return "Stopped"
	case Paused:
		return "Paused"
	case Pending:
		return "Waiting for capacity"
	case Failed:
		return "Failed"
	}
	return fmt.Sprintf("%s → %s", from, to)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
