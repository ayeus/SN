package main

import (
	"context"
	"fmt"
	"time"

	agentv1 "github.com/ayeus/ayeusann/gen/go/agent/v1"
	"github.com/ayeus/ayeusann/internal/lifecycle"
	"github.com/ayeus/ayeusann/internal/placement"
	"github.com/jackc/pgx/v5"
)

// Loop timings. UML §8: an agent that does not confirm a manifest within the
// reservation TTL (60 s) loses the reservation and the scheduler re-scores.
// PRD F-11: pause/stop drains with a 30 s grace period.
const (
	loopInterval   = time.Second
	confirmTimeout = 60 * time.Second
	stopGrace      = 30 * time.Second
)

// runLoops drives dispatch, stop and liveness until ctx ends.
func (s *AgentServer) runLoops(ctx context.Context, heartbeatTimeout time.Duration) {
	t := time.NewTicker(loopInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, step := range []struct {
			name string
			fn   func(context.Context) error
		}{
			{"dispatch", s.dispatchPending},
			{"confirm-timeout", s.expireUnconfirmed},
			{"stop", s.sendStops},
			{"sweep", func(ctx context.Context) error { return s.sweepOffline(ctx, heartbeatTimeout) }},
		} {
			if err := step.fn(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("coordinator loop step failed", "step", step.name, "err", err)
			}
		}
	}
}

type pendingReplica struct {
	id, deploymentID, hostID, modelID, modelName, runtime string
	gpuUUID                                               *string
	refs                                                  []byte
}

// dispatchPending sends a signed manifest for every placed replica whose host
// is connected and which has not been dispatched yet.
func (s *AgentServer) dispatchPending(ctx context.Context) error {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT r.id, r.deployment_id, r.host_id, d.model_id, m.name, COALESCE(h.runtime, ''), g.uuid, m.runtime_refs
		FROM replicas r
		JOIN deployments d ON d.id = r.deployment_id
		JOIN models m ON m.id = d.model_id
		JOIN hosts h ON h.id = r.host_id
		LEFT JOIN gpus g ON g.id = r.gpu_id
		WHERE r.state = 'pending' AND r.dispatched_at IS NULL
		ORDER BY r.created_at
		LIMIT 100;
	`)
	if err != nil {
		return err
	}
	var todo []pendingReplica
	for rows.Next() {
		var p pendingReplica
		if err := rows.Scan(&p.id, &p.deploymentID, &p.hostID, &p.modelID, &p.modelName, &p.runtime, &p.gpuUUID, &p.refs); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, p := range todo {
		sess := s.sessions.get(p.hostID)
		if sess == nil {
			continue // host not connected; the confirm timeout reclaims the slot
		}

		refs, err := placement.ParseRuntimeRefs(p.refs)
		ref, ok := refs[p.runtime]
		if err != nil || !ok || ref.Model == "" {
			s.failReplica(ctx, p.id, fmt.Sprintf("model %s is not packaged for the host's %q runtime", p.modelName, p.runtime))
			continue
		}

		m := &agentv1.ManifestDispatch{
			JobId:        p.id,
			ReplicaId:    p.id,
			DeploymentId: p.deploymentID,
			ModelId:      p.modelID,
			ModelName:    p.modelName,
			RuntimeModel: ref.Model,
			IssuedAt:     time.Now().Unix(),
		}
		if p.gpuUUID != nil {
			m.GpuUuid = *p.gpuUUID
		}
		s.signer.Sign(m)

		// Claim the dispatch before sending so a slow send cannot race the
		// next tick into sending twice.
		tag, err := s.db.Pool.Exec(ctx, `
			UPDATE replicas SET dispatched_at = NOW(), detail = 'job sent to host', updated_at = NOW()
			WHERE id = $1 AND dispatched_at IS NULL AND state = 'pending';
		`, p.id)
		if err != nil || tag.RowsAffected() == 0 {
			continue
		}
		if err := sess.send(&agentv1.CoordinatorMessage{Payload: &agentv1.CoordinatorMessage_Manifest{Manifest: m}}); err != nil {
			s.log.Warn("manifest send failed; will retry", "replica_id", p.id, "err", err)
			_, _ = s.db.Pool.Exec(ctx, `UPDATE replicas SET dispatched_at = NULL WHERE id = $1;`, p.id)
			continue
		}
		rid := p.id
		_ = lifecycle.Event(ctx, s.db.Pool, p.deploymentID, &rid, "info", "",
			fmt.Sprintf("Signed manifest sent to host (%s via %s)", ref.Model, p.runtime))
		s.log.Info("manifest dispatched", "replica_id", p.id, "host_id", p.hostID, "runtime_model", ref.Model)
	}
	return nil
}

// expireUnconfirmed fails replicas whose host never acknowledged the manifest,
// or that were placed on a host that never connected. The scheduler then
// places a replacement on the next-best candidate.
func (s *AgentServer) expireUnconfirmed(ctx context.Context) error {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT id, dispatched_at IS NOT NULL FROM replicas
		WHERE state = 'pending'
		  AND COALESCE(dispatched_at, created_at) < NOW() - ($1 * INTERVAL '1 second');
	`, int(confirmTimeout.Seconds()))
	if err != nil {
		return err
	}
	type exp struct {
		id         string
		dispatched bool
	}
	var list []exp
	for rows.Next() {
		var e exp
		if err := rows.Scan(&e.id, &e.dispatched); err != nil {
			rows.Close()
			return err
		}
		list = append(list, e)
	}
	rows.Close()
	for _, e := range list {
		reason := "host did not confirm the job within 60 s"
		if !e.dispatched {
			reason = "host was not connected within 60 s of placement"
		}
		s.failReplica(ctx, e.id, reason)
	}
	return rows.Err()
}

// sendStops asks agents to tear down replicas the scheduler marked STOPPING.
// A replica whose host is gone, or that is not confirmed within the grace
// period, is marked STOPPED: nothing is left running that we could reach.
func (s *AgentServer) sendStops(ctx context.Context) error {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT id, host_id, stop_sent_at IS NOT NULL, COALESCE(stop_sent_at < NOW() - ($1 * INTERVAL '1 second'), FALSE)
		FROM replicas WHERE state = 'stopping';
	`, int(stopGrace.Seconds()))
	if err != nil {
		return err
	}
	type st struct {
		id, hostID    string
		sent, overdue bool
	}
	var list []st
	for rows.Next() {
		var x st
		if err := rows.Scan(&x.id, &x.hostID, &x.sent, &x.overdue); err != nil {
			rows.Close()
			return err
		}
		list = append(list, x)
	}
	rows.Close()

	for _, x := range list {
		sess := s.sessions.get(x.hostID)
		switch {
		case sess == nil:
			s.setReplica(ctx, x.id, lifecycle.Stopped, "host offline; nothing left running", "")
		case x.overdue:
			s.setReplica(ctx, x.id, lifecycle.Stopped, "stop not confirmed within 30 s grace period", "")
		case !x.sent:
			if err := sess.send(&agentv1.CoordinatorMessage{Payload: &agentv1.CoordinatorMessage_StopReplica{
				StopReplica: &agentv1.StopReplica{ReplicaId: x.id, Reason: "deployment stopped"},
			}}); err == nil {
				_, _ = s.db.Pool.Exec(ctx, `UPDATE replicas SET stop_sent_at = NOW() WHERE id = $1;`, x.id)
			}
		}
	}
	return rows.Err()
}

// sweepOffline marks hosts that missed three heartbeats OFFLINE (SRS FR-40) and
// fails their replicas so the scheduler backfills elsewhere (FR-43).
func (s *AgentServer) sweepOffline(ctx context.Context, timeout time.Duration) error {
	rows, err := s.db.Pool.Query(ctx, `
		UPDATE hosts SET status = 'offline', runtime_healthy = FALSE, updated_at = NOW()
		WHERE deleted_at IS NULL
		  AND status IN ('registered', 'benchmarking', 'probation', 'active', 'draining', 'demoted')
		  AND (last_heartbeat_at IS NULL OR last_heartbeat_at < NOW() - ($1 * INTERVAL '1 second'))
		RETURNING id;
	`, int(timeout.Seconds()))
	if err != nil {
		return err
	}
	var hosts []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		hosts = append(hosts, id)
	}
	rows.Close()

	for _, hostID := range hosts {
		s.log.Warn("host missed heartbeats; marking offline", "host_id", hostID)
		if sess := s.sessions.get(hostID); sess != nil {
			sess.close()
		}
		reps, err := s.db.Pool.Query(ctx, `
			SELECT id FROM replicas
			WHERE host_id = $1 AND state IN ('pending', 'pulling', 'loading', 'warming', 'serving', 'degraded', 'stopping');
		`, hostID)
		if err != nil {
			return err
		}
		var ids []string
		for reps.Next() {
			var id string
			if err := reps.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		reps.Close()
		for _, id := range ids {
			s.failReplica(ctx, id, "host went offline (missed 3 heartbeats)")
		}
	}
	return rows.Err()
}

func (s *AgentServer) failReplica(ctx context.Context, replicaID, reason string) {
	s.setReplica(ctx, replicaID, lifecycle.Failed, "", reason)
}

// setReplica applies a coordinator-observed replica state and recomputes the
// deployment in one transaction.
func (s *AgentServer) setReplica(ctx context.Context, replicaID, state, detail, errMsg string) {
	err := s.db.ExecTx(ctx, func(tx pgx.Tx) error {
		depID, _, err := lifecycle.SetReplicaState(ctx, tx, replicaID, state, detail, errMsg)
		if err != nil || depID == "" {
			return err
		}
		_, _, err = lifecycle.Recompute(ctx, tx, depID)
		return err
	})
	if err != nil {
		s.log.Error("replica state update failed", "replica_id", replicaID, "state", state, "err", err)
	}
}
