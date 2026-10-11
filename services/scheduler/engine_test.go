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

// llama-3.1-8b-instruct: another catalogue model an 8 GB GPU can run.
const otherModel = "550e8400-e29b-41d4-a716-446655440001"

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
		heartbeatTimeout: 15 * time.Second,
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

// On a network of a few machines the GPU a deployment needs is often just
// switched off. Waiting for it is not a failure, however long it takes.
func TestWaitingForCapacityNeverBecomesAFailure(t *testing.T) {
	f := setup(t)
	dep := f.deployment("t3", 1)

	f.tick()
	if s := f.state(dep); s != "pending" {
		t.Fatalf("state = %s, want pending", s)
	}
	events := f.count(`SELECT COUNT(*) FROM deployment_events WHERE deployment_id = $1`, dep)
	f.tick()
	if n := f.count(`SELECT COUNT(*) FROM deployment_events WHERE deployment_id = $1`, dep); n != events {
		t.Fatalf("events grew from %d to %d: the same waiting reason must be logged once", events, n)
	}

	f.exec(`UPDATE deployments SET state_changed_at = NOW() - INTERVAL '3 days', placing_since = NOW() - INTERVAL '3 days' WHERE id = $1`, dep)
	f.tick()
	if s := f.state(dep); s != "pending" {
		t.Fatalf("state = %s after three days without capacity, want still pending", s)
	}

	// And when a machine does turn up, it is used.
	f.host("t3", 8)
	f.tick()
	if n := f.activeReplicas(dep); n != 1 {
		t.Fatalf("%d replicas placed once capacity arrived, want 1", n)
	}
}

// startFailure records what the coordinator records when a host cannot start
// a replica: failed, never served, GPU free again.
func (f *fixture) startFailure(dep, reason string, ago time.Duration) (host string) {
	f.t.Helper()
	var replica string
	if err := testDB.Pool.QueryRow(f.ctx, `
		SELECT id, host_id FROM replicas WHERE deployment_id = $1 AND state = 'pending'`, dep).Scan(&replica, &host); err != nil {
		f.t.Fatalf("no pending replica to fail: %v", err)
	}
	f.exec(`UPDATE replicas SET state = 'failed', last_error = $2, stopped_at = NOW() - ($3 * INTERVAL '1 second') WHERE id = $1`,
		replica, reason, int(ago.Seconds()))
	f.exec(`UPDATE gpus SET status = 'available', replica_id = NULL WHERE replica_id = $1`, replica)
	return host
}

// A machine that cannot start a model is still the best-scoring machine a
// second later. It must not be handed the same job again and again.
func TestAMachineThatFailsToStartAModelIsRested(t *testing.T) {
	f := setup(t)
	f.host("t3", 8)
	dep := f.deployment("t3", 1)
	f.tick()
	bad := f.startFailure(dep, "pull failed: no space left on device", 0)

	f.tick()
	if n := f.activeReplicas(dep); n != 0 {
		t.Fatal("the replica was placed straight back on the machine that had just failed to start it")
	}
	var why string
	f.scan(&why, `SELECT COALESCE(last_error, '') FROM deployments WHERE id = $1`, dep)
	if !strings.Contains(why, "failed to start this model") {
		t.Fatalf("the deployment says %q, want it to say the machine is being rested", why)
	}

	// Another model is that machine's business as usual.
	var other string
	f.scan(&other, `
		INSERT INTO deployments (org_id, model_id, name, tier, region, min_replicas, max_replicas)
		VALUES ($1, $2, 'other', 't3', 'IN-SOUTH', 1, 1) RETURNING id`, f.org, otherModel)
	f.tick()
	if n := f.activeReplicas(other); n != 1 {
		t.Fatal("a machine resting for one model was refused a different one")
	}
	f.exec(`UPDATE deployments SET desired_state = 'stopped' WHERE id = $1`, other)
	f.exec(`UPDATE replicas SET state = 'stopped' WHERE deployment_id = $1`, other)
	f.exec(`UPDATE gpus SET status = 'available', replica_id = NULL`)

	// After the first cool-down it is tried again,
	f.exec(`UPDATE replicas SET stopped_at = NOW() - INTERVAL '61 seconds' WHERE deployment_id = $1 AND state = 'failed'`, dep)
	f.tick()
	if n := f.activeReplicas(dep); n != 1 {
		t.Fatal("the machine was not tried again after its cool-down")
	}
	// and a second failure in a row earns a longer one.
	if again := f.startFailure(dep, "pull failed: no space left on device", 61*time.Second); again != bad {
		t.Fatal("expected the retry on the same machine")
	}
	f.tick()
	if n := f.activeReplicas(dep); n != 0 {
		t.Fatal("after two failures in a row the machine was tried again after only one minute")
	}
	if cooldownAfter(1) != time.Minute || cooldownAfter(2) != 2*time.Minute || cooldownAfter(40) != maxCooldown {
		t.Fatalf("cool-downs %s, %s, %s", cooldownAfter(1), cooldownAfter(2), cooldownAfter(40))
	}
}

// One machine failing is that machine's problem. Three different machines
// failing says the model or the request is, and the deployment stops trying
// and says why.
func TestADeploymentIsGivenUpAfterThreeMachinesFailToStartIt(t *testing.T) {
	f := setup(t)
	for range 4 {
		f.host("t3", 8)
	}
	dep := f.deployment("t3", 1)

	hosts := map[string]bool{}
	for i := range 3 {
		f.tick()
		if s := f.state(dep); s == "failed" {
			t.Fatalf("given up after %d machine(s), want three", i)
		}
		hosts[f.startFailure(dep, "model requires more system memory than is available", 0)] = true
	}
	if len(hosts) != 3 {
		t.Fatalf("the three attempts used %d machines, want each on a different one", len(hosts))
	}

	f.tick()
	if s := f.state(dep); s != "failed" {
		t.Fatalf("state = %s after three machines failed, want failed", s)
	}
	var why string
	f.scan(&why, `SELECT last_error FROM deployments WHERE id = $1`, dep)
	if !strings.Contains(why, "3 machines") || !strings.Contains(why, "more system memory") {
		t.Fatalf("the reason given is %q, want the count and the last machine's error", why)
	}
	if n := f.activeReplicas(dep); n != 0 {
		t.Fatal("a fourth machine was tried after the deployment was given up")
	}

	// Retrying starts the count again: the old failures are not held against it.
	f.exec(`UPDATE deployments SET state = 'pending', last_error = NULL, placing_since = NOW() WHERE id = $1`, dep)
	f.tick()
	if s := f.state(dep); s == "failed" {
		t.Fatal("a retried deployment was given up at once on the strength of its old failures")
	}
	if n := f.activeReplicas(dep); n != 1 {
		t.Fatalf("%d replicas after the retry, want one on the machine that has not failed", n)
	}
}

// A deployment that has served is not written off for failures from before
// it did.
func TestFailuresBeforeASuccessfulStartAreNotCounted(t *testing.T) {
	f := setup(t)
	for range 4 {
		f.host("t3", 8)
	}
	dep := f.deployment("t3", 1)
	for range 2 {
		f.tick()
		f.startFailure(dep, "warm-up failed", 0)
	}
	// The third machine starts it.
	f.tick()
	f.exec(`UPDATE replicas SET state = 'serving', started_at = NOW() WHERE deployment_id = $1 AND state = 'pending'`, dep)
	f.tick()
	if s := f.state(dep); s != "serving" {
		t.Fatalf("state = %s, want serving", s)
	}
	// Later that machine goes away, and the first replacement fails to start.
	f.exec(`UPDATE replicas SET state = 'failed', stopped_at = NOW() WHERE deployment_id = $1 AND state = 'serving'`, dep)
	f.exec(`UPDATE gpus SET status = 'available', replica_id = NULL`)
	f.tick()
	f.startFailure(dep, "warm-up failed", 0)
	f.tick()
	if s := f.state(dep); s == "failed" {
		t.Fatal("one failure after a week of serving was added to two from before it, and the deployment given up")
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
