package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/testutil"
)

// gemma-2-2b-it from the seed catalogue: Ollama needs 3 GB, every tier may run it.
const smallModel = "550e8400-e29b-41d4-a716-446655440015"

var testDB *db.Client

func TestMain(m *testing.M) {
	client, cleanup, err := testutil.NewIsolatedDB("scheduler")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testDB = client
	code := m.Run()
	cleanup()
	os.Exit(code)
}

type fixture struct {
	t   *testing.T
	ctx context.Context
	r   *Reconciler
	org string
}

// setup returns a reconciler over an empty set of hosts and deployments.
func setup(t *testing.T) *fixture {
	t.Helper()
	if testDB == nil {
		t.Skip("skipping: test database unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	f := &fixture{t: t, ctx: ctx, r: &Reconciler{
		db: testDB, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		heartbeatTimeout: 15 * time.Second, scheduleTimeout: 5 * time.Minute,
	}}
	f.exec(`TRUNCATE deployment_events, replicas, gpus, deployments, hosts CASCADE`)
	f.scan(&f.org, `INSERT INTO organizations (name) VALUES ('SchedulerTestOrg') RETURNING id`)
	return f
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := testDB.Pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", strings.Fields(sql)[0], err)
	}
}

func (f *fixture) scan(dest any, sql string, args ...any) {
	f.t.Helper()
	if err := testDB.Pool.QueryRow(f.ctx, sql, args...).Scan(dest); err != nil {
		f.t.Fatalf("query failed: %v\n%s", err, sql)
	}
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	f.scan(&n, sql, args...)
	return n
}

// host adds an online, healthy Ollama host with one free GPU and returns its id.
func (f *fixture) host(tier string, vramGB int) string {
	f.t.Helper()
	var id string
	f.scan(&id, `
		INSERT INTO hosts (name, tier, region, status, reputation, runtime, runtime_healthy, last_heartbeat_at)
		VALUES ('sched-' || gen_random_uuid(), $1, 'IN-SOUTH', 'active', 80, 'ollama', TRUE, NOW()) RETURNING id`, tier)
	f.exec(`INSERT INTO gpus (host_id, model, vram_gb, uuid) VALUES ($1, 'Test GPU', $2, 'GPU-' || gen_random_uuid())`, id, vramGB)
	return id
}

func (f *fixture) deployment(tier string, replicas int) string {
	f.t.Helper()
	var id string
	f.scan(&id, `
		INSERT INTO deployments (org_id, model_id, name, tier, region, min_replicas, max_replicas)
		VALUES ($1, $2, 'dep-' || gen_random_uuid(), $3, 'IN-SOUTH', $4, $4) RETURNING id`, f.org, smallModel, tier, replicas)
	return id
}

func (f *fixture) tick() {
	f.t.Helper()
	if err := f.r.Tick(f.ctx); err != nil {
		f.t.Fatalf("tick: %v", err)
	}
}

func (f *fixture) state(dep string) string {
	f.t.Helper()
	var s string
	f.scan(&s, `SELECT state FROM deployments WHERE id = $1`, dep)
	return s
}

func (f *fixture) activeReplicas(dep string) int {
	return f.count(`SELECT COUNT(*) FROM replicas WHERE deployment_id = $1 AND state NOT IN ('stopped', 'failed')`, dep)
}

func TestPlacesReplicasOnDistinctHostsAndReservesTheirGPUs(t *testing.T) {
	f := setup(t)
	f.host("t3", 8)
	f.host("t3", 8)
	f.host("t3", 8)
	dep := f.deployment("t3", 2)

	f.tick()

	if n := f.activeReplicas(dep); n != 2 {
		t.Fatalf("replicas = %d, want 2", n)
	}
	if n := f.count(`SELECT COUNT(DISTINCT host_id) FROM replicas WHERE deployment_id = $1`, dep); n != 2 {
		t.Fatalf("replicas landed on %d hosts, want 2 distinct", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM gpus g JOIN replicas r ON r.id = g.replica_id WHERE g.status = 'reserved' AND r.deployment_id = $1`, dep); n != 2 {
		t.Fatalf("reserved GPUs = %d, want 2", n)
	}
	if s := f.state(dep); s != "scheduling" {
		t.Fatalf("deployment state = %s, want scheduling", s)
	}

	// A second pass must not add replicas to a satisfied deployment.
	f.tick()
	if n := f.activeReplicas(dep); n != 2 {
		t.Fatalf("after a second tick replicas = %d, want 2", n)
	}
}

func TestOneGPUIsNeverGivenToTwoDeployments(t *testing.T) {
	f := setup(t)
	f.host("t3", 8)
	a := f.deployment("t3", 1)
	b := f.deployment("t3", 1)

	f.tick()

	if na, nb := f.activeReplicas(a), f.activeReplicas(b); na+nb != 1 {
		t.Fatalf("replicas placed = %d + %d, want exactly one for the single GPU", na, nb)
	}
	if n := f.count(`SELECT COUNT(*) FROM gpus WHERE status = 'reserved'`); n != 1 {
		t.Fatalf("reserved GPUs = %d, want 1", n)
	}
}

// Two scheduler processes may tick at the same moment. The GPU claim is what
// keeps them from double-booking a device.
func TestConcurrentReconcilersDoNotDoubleBook(t *testing.T) {
	f := setup(t)
	for i := 0; i < 3; i++ {
		f.host("t3", 8)
	}
	deps := []string{f.deployment("t3", 1), f.deployment("t3", 1), f.deployment("t3", 1)}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.r.Tick(f.ctx)
		}()
	}
	wg.Wait()
	f.tick()

	if n := f.count(`SELECT COUNT(*) FROM (SELECT gpu_id FROM replicas WHERE state NOT IN ('stopped', 'failed') GROUP BY gpu_id HAVING COUNT(*) > 1) x`); n != 0 {
		t.Fatalf("%d GPUs carry more than one active replica", n)
	}
	for _, d := range deps {
		if n := f.activeReplicas(d); n != 1 {
			t.Fatalf("deployment %s has %d replicas, want 1", d, n)
		}
	}
}

func TestTierAndMemoryRulesAreEnforced(t *testing.T) {
	f := setup(t)
	f.host("t3", 8) // wrong tier for a T2 deployment
	f.host("t2", 2) // too little memory for the model
	dep := f.deployment("t2", 1)

	f.tick()

	if n := f.activeReplicas(dep); n != 0 {
		t.Fatalf("replicas = %d, want 0: no host satisfies the deployment", n)
	}
	var lastErr *string
	f.scan(&lastErr, `SELECT last_error FROM deployments WHERE id = $1`, dep)
	if lastErr == nil || *lastErr == "" {
		t.Fatal("a deployment waiting for capacity must say why")
	}
	if s := f.state(dep); s != "pending" {
		t.Fatalf("state = %s, want pending", s)
	}

	f.host("t2", 24)
	f.tick()
	if n := f.activeReplicas(dep); n != 1 {
		t.Fatalf("replicas = %d after a suitable host joined, want 1", n)
	}
	f.scan(&lastErr, `SELECT last_error FROM deployments WHERE id = $1`, dep)
	if lastErr != nil {
		t.Fatalf("last_error = %q after placement, want it cleared", *lastErr)
	}
}

func TestHostsThatCannotTakeWorkAreSkipped(t *testing.T) {
	f := setup(t)
	stale := f.host("t3", 8)
	f.exec(`UPDATE hosts SET last_heartbeat_at = NOW() - INTERVAL '1 minute' WHERE id = $1`, stale)
	paused := f.host("t3", 8)
	f.exec(`UPDATE hosts SET paused = TRUE WHERE id = $1`, paused)
	unhealthy := f.host("t3", 8)
	f.exec(`UPDATE hosts SET runtime_healthy = FALSE WHERE id = $1`, unhealthy)
	disreputable := f.host("t3", 8)
	f.exec(`UPDATE hosts SET reputation = 10 WHERE id = $1`, disreputable)
	dep := f.deployment("t3", 1)

	f.tick()

	if n := f.activeReplicas(dep); n != 0 {
		t.Fatalf("replicas = %d, want 0: every host is stale, paused, unhealthy or below the reputation floor", n)
	}
}

func TestNoCapacityFailsTheDeploymentAfterTheTimeout(t *testing.T) {
	f := setup(t)
	dep := f.deployment("t3", 1)

	f.tick()
	if s := f.state(dep); s != "pending" {
		t.Fatalf("state = %s, want pending while inside the timeout", s)
	}
	events := f.count(`SELECT COUNT(*) FROM deployment_events WHERE deployment_id = $1`, dep)
	f.tick()
	if n := f.count(`SELECT COUNT(*) FROM deployment_events WHERE deployment_id = $1`, dep); n != events {
		t.Fatalf("events grew from %d to %d: the same waiting reason must be logged once", events, n)
	}

	f.exec(`UPDATE deployments SET state_changed_at = NOW() - INTERVAL '10 minutes' WHERE id = $1`, dep)
	f.tick()
	if s := f.state(dep); s != "failed" {
		t.Fatalf("state = %s, want failed after the scheduling timeout", s)
	}

	// Failed is sticky: capacity arriving later must not start it on its own.
	f.host("t3", 8)
	f.tick()
	if n := f.activeReplicas(dep); n != 0 {
		t.Fatalf("a failed deployment was placed (%d replicas) without the customer retrying", n)
	}
}

func TestFailedReplicaIsReplacedOnAnotherHost(t *testing.T) {
	f := setup(t)
	f.host("t3", 8)
	f.host("t3", 8)
	dep := f.deployment("t3", 1)
	f.tick()

	var replica, firstHost string
	if err := testDB.Pool.QueryRow(f.ctx, `SELECT id, host_id FROM replicas WHERE deployment_id = $1`, dep).Scan(&replica, &firstHost); err != nil {
		t.Fatal(err)
	}
	// What the coordinator does when a host drops off: fail the replica, free the GPU.
	f.exec(`UPDATE replicas SET state = 'failed' WHERE id = $1`, replica)
	f.exec(`UPDATE gpus SET status = 'available', replica_id = NULL WHERE replica_id = $1`, replica)
	f.exec(`UPDATE hosts SET status = 'offline' WHERE id = $1`, firstHost)

	f.tick()

	var newHost string
	f.scan(&newHost, `SELECT host_id FROM replicas WHERE deployment_id = $1 AND state = 'pending'`, dep)
	if newHost == firstHost {
		t.Fatal("the replacement was placed on the host that went offline")
	}
}

func TestStoppingADeploymentDrainsItsReplicas(t *testing.T) {
	f := setup(t)
	f.host("t3", 8)
	f.host("t3", 8)
	dep := f.deployment("t3", 2)
	f.tick()

	// One replica reached the host and is serving; the other was never sent.
	var serving string
	f.scan(&serving, `SELECT id FROM replicas WHERE deployment_id = $1 LIMIT 1`, dep)
	f.exec(`UPDATE replicas SET state = 'serving', dispatched_at = NOW() WHERE id = $1`, serving)
	f.exec(`UPDATE deployments SET desired_state = 'stopped' WHERE id = $1`, dep)

	f.tick()

	var st string
	f.scan(&st, `SELECT state FROM replicas WHERE id = $1`, serving)
	if st != "stopping" {
		t.Fatalf("serving replica is %s, want stopping (the host must be told to stop it)", st)
	}
	if n := f.count(`SELECT COUNT(*) FROM replicas WHERE deployment_id = $1 AND id <> $2 AND state = 'stopped'`, dep, serving); n != 1 {
		t.Fatal("a replica that was never dispatched must stop immediately")
	}
	if n := f.count(`SELECT COUNT(*) FROM gpus WHERE status = 'reserved'`); n != 1 {
		t.Fatalf("reserved GPUs = %d, want 1: only the replica still stopping holds its GPU", n)
	}
	if s := f.state(dep); s != "stopping" {
		t.Fatalf("deployment state = %s, want stopping", s)
	}

	// The coordinator confirms the stop; the deployment settles and is left alone.
	f.exec(`UPDATE replicas SET state = 'stopped' WHERE id = $1`, serving)
	f.tick()
	if s := f.state(dep); s != "stopped" {
		t.Fatalf("deployment state = %s, want stopped", s)
	}
	f.tick()
	if n := f.activeReplicas(dep); n != 0 {
		t.Fatalf("a stopped deployment has %d active replicas", n)
	}
}
