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

	// livenessGrace is how long hosts get to reconnect before anyone is
	// declared offline, after this process starts or after it stalled. Agents
	// back off up to 30 s between attempts, so 90 s covers a retry or two.
	livenessGrace = 90 * time.Second
	// stallAfter is the gap between two liveness checks that counts as a stall:
	// the computer slept, the database was away, the process was frozen. Every
	// heartbeat on record is then stale through no fault of the hosts.
	stallAfter = 10 * time.Second
	// returnGrace is how long a serving replica waits for its unreachable host
	// before it is given up and replaced.
	returnGrace = 60 * time.Second
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
		s.tick(ctx, heartbeatTimeout)
	}
}

// tick runs every loop step once.
func (s *AgentServer) tick(ctx context.Context, heartbeatTimeout time.Duration) {
	type step struct {
		name string
		fn   func(context.Context) error
	}
	steps := []step{
		{"dispatch", s.dispatchPending},
		{"stop", s.sendStops},
	}
	// The steps that conclude something from a host's silence wait out the
	// grace window: right after a start or a stall, silence says nothing
	// about the hosts.
	if !s.inGrace() {
		steps = append(steps,
			step{"confirm-timeout", s.expireUnconfirmed},
			step{"sweep", func(ctx context.Context) error { return s.sweepOffline(ctx, heartbeatTimeout) }},
			step{"give-up", s.failUnreturned},
		)
	}
	failed := false
	for _, st := range steps {
		if err := st.fn(ctx); err != nil && ctx.Err() == nil {
			failed = true
			s.log.Error("coordinator loop step failed", "step", st.name, "err", err)
		}
	}
	// A pass only counts when it could reach the database. One that could not
	// leaves lastTick alone, so the next good one sees the gap and starts a
	// grace window: heartbeats could not be recorded either.
	if !failed {
		s.lastTick = s.now()
	}
}

// now is the wall clock, with Go's monotonic reading stripped: a monotonic
// clock does not advance while the computer sleeps, and a sleep is exactly the
// gap inGrace has to see.
func (s *AgentServer) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now().Round(0)
}

// inGrace reports whether hosts are still being given time to reconnect, and
// opens a new grace window when this is the first check, or the first after a
// stall.
func (s *AgentServer) inGrace() bool {
	now := s.now()
	if gap := now.Sub(s.lastTick); s.lastTick.IsZero() || gap > stallAfter || gap < 0 {
		s.graceUntil = now.Add(livenessGrace)
		if s.lastTick.IsZero() {
			s.log.Info("coordinator started; hosts have a grace period to reconnect", "seconds", int(livenessGrace.Seconds()))
		} else {
			s.log.Warn("the control loop stalled (sleep, a frozen process or an unreachable database); hosts have a grace period to reconnect",
				"stalled_for", gap.Round(time.Second).String(), "seconds", int(livenessGrace.Seconds()))
		}
		// Mark it now, so the window is opened once rather than on every pass
		// until the first one that completes.
		s.lastTick = now
	}
	return now.Before(s.graceUntil)
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
			// A stream that cannot be written to is finished. Dropping the
			// session makes the agent reconnect, and the job is sent then.
			s.log.Warn("manifest send failed; dropping the session", "replica_id", p.id, "host_id", p.hostID, "err", err)
			s.sessions.drop(p.hostID)
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
			// Nobody to tell. If the host is only away and still holds the
			// model, it says so when it reconnects and is told to stop then
			// (reconcileOnRegister), so this is safe even while hosts are
			// still finding their way back after a restart.
			s.setReplica(ctx, x.id, lifecycle.Stopped, "host not connected; it is told to stop if it returns", "")
		case x.overdue:
			s.setReplica(ctx, x.id, lifecycle.Stopped, "stop not confirmed within 30 s grace period", "")
		case !x.sent:
			if err := sess.send(&agentv1.CoordinatorMessage{Payload: &agentv1.CoordinatorMessage_StopReplica{
				StopReplica: &agentv1.StopReplica{ReplicaId: x.id, Reason: "deployment stopped"},
			}}); err == nil {
				_, _ = s.db.Pool.Exec(ctx, `UPDATE replicas SET stop_sent_at = NOW() WHERE id = $1;`, x.id)
			} else {
				// The stream is finished; the next pass finds no session and
				// closes the replica out.
				s.log.Warn("stop send failed; dropping the session", "replica_id", x.id, "host_id", x.hostID, "err", err)
				s.sessions.drop(x.hostID)
			}
		}
	}
	return rows.Err()
}

// sweepOffline marks hosts that missed three heartbeats OFFLINE (SRS FR-40).
//
// What happens to their replicas depends on how far each had got. One that
// was still starting is failed at once, so the scheduler places it elsewhere
// (FR-43). One that was serving is only marked DEGRADED: the model is very
// likely still loaded on a machine whose Wi-Fi blinked, and if the host is
// back within returnGrace it carries on without reloading anything.
// failUnreturned gives up on the ones whose host stays away.
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
		// End its connection, if it still has one: an agent that is alive but
		// was not heard will see the stream close and reconnect.
		s.sessions.drop(hostID)
		reps, err := s.db.Pool.Query(ctx, `
			SELECT id, state FROM replicas
			WHERE host_id = $1 AND state IN ('pending', 'pulling', 'loading', 'warming', 'serving');
		`, hostID)
		if err != nil {
			return err
		}
		type rep struct{ id, state string }
		var list []rep
		for reps.Next() {
			var r rep
			if err := reps.Scan(&r.id, &r.state); err == nil {
				list = append(list, r)
			}
		}
		reps.Close()
		for _, r := range list {
			if r.state == lifecycle.Serving {
				s.setReplica(ctx, r.id, lifecycle.Degraded,
					fmt.Sprintf("host unreachable; waiting %d s for it to return", int(returnGrace.Seconds())), "")
			} else {
				s.failReplica(ctx, r.id, "host went offline (missed 3 heartbeats)")
			}
		}
	}
	return rows.Err()
}

// failUnreturned gives up on replicas whose host went offline and did not
// come back within returnGrace, so the scheduler replaces them.
func (s *AgentServer) failUnreturned(ctx context.Context) error {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT r.id FROM replicas r JOIN hosts h ON h.id = r.host_id
		WHERE r.state = 'degraded' AND h.status = 'offline'
		  AND r.updated_at < NOW() - ($1 * INTERVAL '1 second');
	`, int(returnGrace.Seconds()))
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		s.failReplica(ctx, id, fmt.Sprintf("host went offline and did not return within %d s", int(returnGrace.Seconds())))
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
