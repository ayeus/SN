package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/ayeus/ayeusann/gen/go/agent/v1"
	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/manifest"
	"github.com/ayeus/ayeusann/internal/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// gemma-2-2b-it from the seed catalogue, packaged for Ollama as gemma2:2b.
const smallModel = "550e8400-e29b-41d4-a716-446655440015"

var testDB *db.Client

func TestMain(m *testing.M) {
	client, cleanup, err := testutil.NewIsolatedDB("coordinator")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testDB = client
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// fakeStream stands in for an agent's gRPC stream: the test feeds it agent
// messages and reads what the coordinator sent.
type fakeStream struct {
	grpc.ServerStream
	ctx    context.Context
	cancel context.CancelFunc
	in     chan *agentv1.AgentMessage

	mu   sync.Mutex
	sent []*agentv1.CoordinatorMessage
	out  chan *agentv1.CoordinatorMessage
}

func newStream() *fakeStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeStream{ctx: ctx, cancel: cancel, in: make(chan *agentv1.AgentMessage, 16), out: make(chan *agentv1.CoordinatorMessage, 64)}
}

func (f *fakeStream) Context() context.Context { return f.ctx }

func (f *fakeStream) Send(m *agentv1.CoordinatorMessage) error {
	f.mu.Lock()
	f.sent = append(f.sent, m)
	f.mu.Unlock()
	select {
	case f.out <- m:
	default:
	}
	return nil
}

func (f *fakeStream) Recv() (*agentv1.AgentMessage, error) {
	select {
	case m, ok := <-f.in:
		if !ok {
			return nil, io.EOF
		}
		return m, nil
	case <-f.ctx.Done():
		return nil, status.Error(codes.Canceled, "stream closed")
	}
}

func (f *fakeStream) messages() []*agentv1.CoordinatorMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*agentv1.CoordinatorMessage(nil), f.sent...)
}

type fixture struct {
	t         *testing.T
	ctx       context.Context
	s         *AgentServer
	org, user string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	if testDB == nil {
		t.Skip("skipping: test database unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	tm, err := auth.NewTokenManager("coordinator-test-secret-0123456789abcdef", 15*time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := manifest.NewSigner("", "coordinator-test")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, ctx: ctx, s: &AgentServer{
		db: testDB, tm: tm, revocations: auth.NewPGRevocationStore(testDB.Pool), signer: signer,
		sessions: newRegistry(), log: slog.New(slog.NewTextHandler(io.Discard, nil)), probationDays: 7,
	}}
	f.exec(`TRUNCATE deployment_events, replicas, gpus, benchmarks, host_telemetry, host_registration_tokens,
	        overlay_ip_allocations, deployments, hosts CASCADE`)
	f.scan(&f.org, `INSERT INTO organizations (name) VALUES ('CoordinatorTestOrg') RETURNING id`)
	f.scan(&f.user, `INSERT INTO users (email, name) VALUES ('coord-' || gen_random_uuid() || '@example.com', 'Host Owner') RETURNING id`)
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

func (f *fixture) str(sql string, args ...any) string {
	f.t.Helper()
	var s string
	f.scan(&s, sql, args...)
	return s
}

func (f *fixture) token(tier string) string {
	f.t.Helper()
	tok, err := f.s.tm.GenerateRegistrationToken(f.user, f.org, tier)
	if err != nil {
		f.t.Fatal(err)
	}
	return tok.Token
}

func gpu(vramGB int32) []*agentv1.GpuInfo {
	return []*agentv1.GpuInfo{{Model: "Test GPU", VramGb: vramGB, Uuid: "GPU-test-0"}}
}

func (f *fixture) register(tier, fingerprint string, vramGB int32) *agentv1.RegisterRequest {
	return &agentv1.RegisterRequest{
		RegistrationToken: f.token(tier), Hostname: "box", Region: "IN-SOUTH", Gpus: gpu(vramGB),
		HardwareFingerprint: fingerprint, Runtime: "ollama", AgentVersion: "0.3.0",
	}
}

// host adds an online Ollama host with one GPU, as if enrolled and benchmarked.
func (f *fixture) host() (hostID, gpuID string) {
	f.t.Helper()
	f.scan(&hostID, `
		INSERT INTO hosts (name, tier, region, status, runtime, runtime_healthy, last_heartbeat_at)
		VALUES ('coord-' || gen_random_uuid(), 't3', 'IN-SOUTH', 'active', 'ollama', TRUE, NOW()) RETURNING id`)
	f.scan(&gpuID, `INSERT INTO gpus (host_id, model, vram_gb, uuid) VALUES ($1, 'Test GPU', 8, 'GPU-' || gen_random_uuid()) RETURNING id`, hostID)
	return hostID, gpuID
}

func (f *fixture) deployment() string {
	f.t.Helper()
	return f.str(`
		INSERT INTO deployments (org_id, model_id, name, tier, region, min_replicas, max_replicas)
		VALUES ($1, $2, 'dep-' || gen_random_uuid(), 't3', 'IN-SOUTH', 1, 1) RETURNING id`, f.org, smallModel)
}

// replica places a pending replica on the host and reserves its GPU, as the
// scheduler does.
func (f *fixture) replica(dep, hostID, gpuID string) string {
	f.t.Helper()
	id := f.str(`INSERT INTO replicas (deployment_id, host_id, gpu_id, state) VALUES ($1, $2, $3, 'pending') RETURNING id`, dep, hostID, gpuID)
	f.exec(`UPDATE gpus SET status = 'reserved', replica_id = $2 WHERE id = $1`, gpuID, id)
	return id
}

// connect installs a live session for the host.
func (f *fixture) connect(hostID string) *fakeStream {
	st := newStream()
	f.t.Cleanup(st.cancel)
	f.s.sessions.put(newSession(hostID, st))
	return st
}

func (f *fixture) replicaState(id string) string {
	return f.str(`SELECT state FROM replicas WHERE id = $1`, id)
}

// ─── Enrolment ───────────────────────────────────────────────

func TestEnrolIssuesACredentialAndRegistersTheGPU(t *testing.T) {
	f := setup(t)
	reg := f.register("t3", "sha256:machine-a", 8)

	en, code, reason := f.s.enrol(f.ctx, reg)
	if code != codes.OK {
		t.Fatalf("enrol refused: %s", reason)
	}
	if !auth.ValidHostCredentialFormat(en.credential) {
		t.Fatalf("credential %q is not a host credential", en.credential)
	}
	if en.status != "benchmarking" || en.tier != "t3" || en.overlayIP == "" {
		t.Fatalf("enrolment = %+v, want benchmarking T3 with an overlay address", en)
	}
	if got := f.str(`SELECT model || ':' || vram_gb || ':' || status FROM gpus WHERE host_id = $1`, en.hostID); got != "Test GPU:8:available" {
		t.Fatalf("GPU row = %s", got)
	}
	if owner := f.str(`SELECT user_id::TEXT FROM hosts WHERE id = $1`, en.hostID); owner != f.user {
		t.Fatal("the host is not owned by the account that created the token")
	}

	// The token is single use.
	if _, code, _ := f.s.enrol(f.ctx, reg); code != codes.Unauthenticated {
		t.Fatalf("a used registration token was accepted (code %s)", code)
	}

	// The credential resumes the session; the raw value is never stored.
	if n := f.str(`SELECT COUNT(*)::TEXT FROM hosts WHERE credential_hash = $1`, en.credential); n != "0" {
		t.Fatal("the raw credential is stored in the database")
	}
	back, code, reason := f.s.resume(f.ctx, &agentv1.RegisterRequest{
		HostCredential: en.credential, HardwareFingerprint: "sha256:machine-a", Gpus: gpu(8), Runtime: "ollama",
	})
	if code != codes.OK || back.hostID != en.hostID {
		t.Fatalf("resume with the issued credential failed: %s", reason)
	}
}

func TestEnrolRefusals(t *testing.T) {
	f := setup(t)
	cases := []struct {
		name string
		edit func(*agentv1.RegisterRequest)
		tier string
		vram int32
		want codes.Code
	}{
		{"garbage token", func(r *agentv1.RegisterRequest) { r.RegistrationToken = "not-a-token" }, "t3", 8, codes.Unauthenticated},
		{"unknown region", func(r *agentv1.RegisterRequest) { r.Region = "MARS-1" }, "t3", 8, codes.InvalidArgument},
		{"no GPU", func(r *agentv1.RegisterRequest) { r.Gpus = nil }, "t3", 8, codes.FailedPrecondition},
		{"no fingerprint", func(r *agentv1.RegisterRequest) { r.HardwareFingerprint = "" }, "t3", 8, codes.InvalidArgument},
		{"implausible VRAM", func(r *agentv1.RegisterRequest) { r.Gpus[0].VramGb = 4096 }, "t3", 8, codes.FailedPrecondition},
		{"old NVIDIA driver", func(r *agentv1.RegisterRequest) {
			r.Gpus[0].Model, r.Gpus[0].DriverVersion = "NVIDIA RTX 3060", "470.82"
		}, "t3", 8, codes.FailedPrecondition},
		{"T2 with a small GPU", func(*agentv1.RegisterRequest) {}, "t2", 8, codes.FailedPrecondition},
	}
	for _, tc := range cases {
		reg := f.register(tc.tier, "sha256:refusal-"+tc.name, tc.vram)
		tc.edit(reg)
		if _, code, _ := f.s.enrol(f.ctx, reg); code != tc.want {
			t.Errorf("%s: code = %s, want %s", tc.name, code, tc.want)
		}
	}
	var hosts int
	f.scan(&hosts, `SELECT COUNT(*) FROM hosts`)
	if hosts != 0 {
		t.Fatalf("%d hosts were created by refused enrolments", hosts)
	}

	// A refusal must not burn the token: fixing the typo and retrying works.
	reg := f.register("t3", "sha256:typo", 8)
	reg.Region = "MARS-1"
	f.s.enrol(f.ctx, reg)
	reg.Region = "IN-SOUTH"
	if _, code, reason := f.s.enrol(f.ctx, reg); code != codes.OK {
		t.Fatalf("token was consumed by a refused attempt: %s", reason)
	}
}

// A development agent pointed at a real installation reports a simulated GPU.
// It must be turned away at enrolment and again on every reconnect, and
// admitted only where the platform says simulated machines are welcome.
func TestSimulatedGPUsAreRefusedUnlessAllowed(t *testing.T) {
	f := setup(t)
	fake := func(reg *agentv1.RegisterRequest) *agentv1.RegisterRequest {
		reg.Gpus = []*agentv1.GpuInfo{{Model: "NVIDIA GeForce RTX 4090", VramGb: 24, Uuid: "GPU-fake-4090-00000000-0001"}}
		return reg
	}

	_, code, reason := f.s.enrol(f.ctx, fake(f.register("t3", "sha256:simulated", 24)))
	if code != codes.FailedPrecondition || !strings.Contains(reason, "simulated GPU") {
		t.Fatalf("a simulated GPU enrolled on a real installation: %s %q", code, reason)
	}
	var hosts int
	f.scan(&hosts, `SELECT COUNT(*) FROM hosts`)
	if hosts != 0 {
		t.Fatalf("the refused machine left %d host rows behind", hosts)
	}

	// Enrolled while it was allowed, then the platform stops allowing it.
	f.s.allowFakeGPU = true
	en, code, reason := f.s.enrol(f.ctx, fake(f.register("t3", "sha256:simulated", 24)))
	if code != codes.OK {
		t.Fatalf("a simulated GPU must enrol where it is allowed: %s %q", code, reason)
	}
	back := fake(&agentv1.RegisterRequest{HostCredential: en.credential, Hostname: "box", HardwareFingerprint: "sha256:simulated", Runtime: "ollama"})
	if _, code, reason := f.s.resume(f.ctx, back); code != codes.OK {
		t.Fatalf("reconnect where simulated GPUs are allowed: %s %q", code, reason)
	}
	f.s.allowFakeGPU = false
	if _, code, _ := f.s.resume(f.ctx, back); code != codes.FailedPrecondition {
		t.Fatalf("a simulated GPU reconnected to a real installation: %s", code)
	}

	// Real hardware is unaffected either way.
	if _, code, reason := f.s.enrol(f.ctx, f.register("t3", "sha256:real", 8)); code != codes.OK {
		t.Fatalf("real hardware was refused: %s %q", code, reason)
	}
}

func TestAMachineBelongsToOneAccount(t *testing.T) {
	f := setup(t)
	first, code, reason := f.s.enrol(f.ctx, f.register("t3", "sha256:shared-machine", 8))
	if code != codes.OK {
		t.Fatal(reason)
	}

	// The same account re-enrolling rotates the credential and keeps the host.
	again, code, reason := f.s.enrol(f.ctx, f.register("t3", "sha256:shared-machine", 8))
	if code != codes.OK || again.hostID != first.hostID {
		t.Fatalf("re-enrolment by the owner failed or made a new host: %s", reason)
	}
	if _, code, _ := f.s.resume(f.ctx, &agentv1.RegisterRequest{HostCredential: first.credential, Gpus: gpu(8)}); code != codes.Unauthenticated {
		t.Fatalf("the old credential still works after re-enrolment (code %s)", code)
	}

	// Another account is refused.
	f.scan(&f.user, `INSERT INTO users (email, name) VALUES ('thief-' || gen_random_uuid() || '@example.com', 'Other') RETURNING id`)
	if _, code, _ := f.s.enrol(f.ctx, f.register("t3", "sha256:shared-machine", 8)); code != codes.PermissionDenied {
		t.Fatalf("another account enrolled an already-enrolled machine (code %s)", code)
	}
}

func TestResumeRefusals(t *testing.T) {
	f := setup(t)
	en, _, _ := f.s.enrol(f.ctx, f.register("t3", "sha256:resume", 8))
	req := func(fp string) *agentv1.RegisterRequest {
		return &agentv1.RegisterRequest{HostCredential: en.credential, HardwareFingerprint: fp, Gpus: gpu(8), Runtime: "ollama"}
	}

	if _, code, _ := f.s.resume(f.ctx, req("sha256:different-hardware")); code != codes.PermissionDenied {
		t.Fatalf("a credential moved to different hardware was accepted (code %s)", code)
	}
	if _, code, _ := f.s.resume(f.ctx, &agentv1.RegisterRequest{HostCredential: "nonsense", Gpus: gpu(8)}); code != codes.Unauthenticated {
		t.Fatalf("a malformed credential was accepted (code %s)", code)
	}

	f.exec(`UPDATE hosts SET status = 'banned' WHERE id = $1`, en.hostID)
	if _, code, _ := f.s.resume(f.ctx, req("sha256:resume")); code != codes.PermissionDenied {
		t.Fatalf("a banned host resumed (code %s)", code)
	}
	if _, code, _ := f.s.enrol(f.ctx, f.register("t3", "sha256:resume", 8)); code != codes.PermissionDenied {
		t.Fatalf("a banned machine re-enrolled (code %s)", code)
	}
	if st := f.str(`SELECT status FROM hosts WHERE id = $1`, en.hostID); st != "banned" {
		t.Fatalf("host status = %s after the refused attempts, want banned", st)
	}
}

func TestBenchmarkMovesTheHostToProbationOrActive(t *testing.T) {
	f := setup(t)
	t3, _, _ := f.s.enrol(f.ctx, f.register("t3", "sha256:bench-t3", 8))
	t1, _, _ := f.s.enrol(f.ctx, f.register("t1", "sha256:bench-t1", 80))
	report := &agentv1.BenchmarkReport{GpuBenchmarks: []*agentv1.GpuBenchmark{{GpuUuid: "GPU-test-0", ComputeScore: 120}}}

	f.s.onBenchmark(f.ctx, t3.hostID, report)
	f.s.onBenchmark(f.ctx, t1.hostID, report)

	if st := f.str(`SELECT status FROM hosts WHERE id = $1`, t3.hostID); st != "probation" {
		t.Fatalf("T3 host is %s after its benchmark, want probation", st)
	}
	if st := f.str(`SELECT status FROM hosts WHERE id = $1`, t1.hostID); st != "active" {
		t.Fatalf("T1 host is %s after its benchmark, want active", st)
	}

	// A later benchmark must not lift a host out of a state an operator set.
	f.exec(`UPDATE hosts SET status = 'banned' WHERE id = $1`, t3.hostID)
	f.s.onBenchmark(f.ctx, t3.hostID, report)
	if st := f.str(`SELECT status FROM hosts WHERE id = $1`, t3.hostID); st != "banned" {
		t.Fatalf("a benchmark changed a banned host to %s", st)
	}
}

// ─── Dispatch, stage events, stop ────────────────────────────

func TestDispatchSendsOneSignedManifest(t *testing.T) {
	f := setup(t)
	hostID, gpuID := f.host()
	rep := f.replica(f.deployment(), hostID, gpuID)

	// Not connected: nothing is sent and the replica keeps waiting.
	if err := f.s.dispatchPending(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.str(`SELECT (dispatched_at IS NULL)::TEXT FROM replicas WHERE id = $1`, rep); got != "true" {
		t.Fatal("a replica was marked dispatched while its host was offline")
	}

	st := f.connect(hostID)
	for i := 0; i < 3; i++ {
		if err := f.s.dispatchPending(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	msgs := st.messages()
	if len(msgs) != 1 {
		t.Fatalf("manifests sent = %d, want exactly 1 across repeated ticks", len(msgs))
	}
	m := msgs[0].GetManifest()
	if m == nil || m.GetReplicaId() != rep || m.GetRuntimeModel() != "gemma2:2b" {
		t.Fatalf("manifest = %+v, want replica %s running gemma2:2b", m, rep)
	}
	if !manifest.Verify(f.s.signer.PublicKey(), m) {
		t.Fatal("the manifest's signature does not verify with the key given to agents")
	}
}

func TestDispatchFailsAReplicaTheHostCannotRun(t *testing.T) {
	f := setup(t)
	hostID, gpuID := f.host()
	f.exec(`UPDATE hosts SET runtime = 'tgi' WHERE id = $1`, hostID)
	rep := f.replica(f.deployment(), hostID, gpuID)
	st := f.connect(hostID)

	if err := f.s.dispatchPending(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s := f.replicaState(rep); s != "failed" {
		t.Fatalf("replica is %s, want failed: the model has no build for the host's runtime", s)
	}
	if len(st.messages()) != 0 {
		t.Fatal("a manifest was sent for a model the host cannot run")
	}
	if s := f.str(`SELECT status FROM gpus WHERE id = $1`, gpuID); s != "available" {
		t.Fatalf("GPU is %s after its replica failed, want available", s)
	}
}

func TestStageEventsRollUpAndAreOwnerChecked(t *testing.T) {
	f := setup(t)
	hostID, gpuID := f.host()
	other, _ := f.host()
	dep := f.deployment()
	rep := f.replica(dep, hostID, gpuID)
	stage := func(from string, s agentv1.ReplicaState) {
		f.s.onStage(f.ctx, from, &agentv1.StageEvent{ReplicaId: rep, State: s})
	}

	stage(other, agentv1.ReplicaState_REPLICA_STATE_SERVING)
	if s := f.replicaState(rep); s != "pending" {
		t.Fatalf("another host moved the replica to %s", s)
	}

	stage(hostID, agentv1.ReplicaState_REPLICA_STATE_PULLING)
	if s := f.str(`SELECT state FROM deployments WHERE id = $1`, dep); s != "pulling" {
		t.Fatalf("deployment state = %s, want pulling", s)
	}
	stage(hostID, agentv1.ReplicaState_REPLICA_STATE_SERVING)
	if s := f.str(`SELECT state FROM deployments WHERE id = $1`, dep); s != "serving" {
		t.Fatalf("deployment state = %s, want serving", s)
	}

	f.s.onStage(f.ctx, hostID, &agentv1.StageEvent{ReplicaId: rep, State: agentv1.ReplicaState_REPLICA_STATE_FAILED, ErrorMessage: "out of memory"})
	if got := f.str(`SELECT state || '/' || last_error FROM replicas WHERE id = $1`, rep); got != "failed/out of memory" {
		t.Fatalf("replica = %s, want failed/out of memory", got)
	}
	if s := f.str(`SELECT status FROM gpus WHERE id = $1`, gpuID); s != "available" {
		t.Fatalf("GPU is %s after the replica failed, want available", s)
	}
	// A late message must not resurrect it.
	stage(hostID, agentv1.ReplicaState_REPLICA_STATE_SERVING)
	if s := f.replicaState(rep); s != "failed" {
		t.Fatalf("a late stage event moved a failed replica to %s", s)
	}
}

func TestUnconfirmedReplicasExpire(t *testing.T) {
	f := setup(t)
	hostID, gpuID := f.host()
	dep := f.deployment()
	fresh := f.replica(dep, hostID, gpuID)
	if err := f.s.expireUnconfirmed(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s := f.replicaState(fresh); s != "pending" {
		t.Fatalf("a replica placed seconds ago was expired (%s)", s)
	}

	f.exec(`UPDATE replicas SET dispatched_at = NOW() - INTERVAL '2 minutes' WHERE id = $1`, fresh)
	if err := f.s.expireUnconfirmed(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s := f.replicaState(fresh); s != "failed" {
		t.Fatalf("replica is %s two minutes after dispatch with no confirmation, want failed", s)
	}
	if s := f.str(`SELECT status FROM gpus WHERE id = $1`, gpuID); s != "available" {
		t.Fatalf("GPU is %s, want available so the scheduler can use it again", s)
	}
}

func TestStopIsSentOnceThenForcedAfterTheGracePeriod(t *testing.T) {
	f := setup(t)
	hostID, gpuID := f.host()
	offlineHost, offlineGPU := f.host()
	dep := f.deployment()
	rep := f.replica(dep, hostID, gpuID)
	gone := f.replica(dep, offlineHost, offlineGPU)
	f.exec(`UPDATE replicas SET state = 'stopping' WHERE deployment_id = $1`, dep)
	st := f.connect(hostID)

	for i := 0; i < 3; i++ {
		if err := f.s.sendStops(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	msgs := st.messages()
	if len(msgs) != 1 || msgs[0].GetStopReplica().GetReplicaId() != rep {
		t.Fatalf("stop messages = %d, want exactly one for the replica", len(msgs))
	}
	if s := f.replicaState(rep); s != "stopping" {
		t.Fatalf("replica is %s inside the grace period, want stopping", s)
	}
	if s := f.replicaState(gone); s != "stopped" {
		t.Fatalf("replica on a disconnected host is %s, want stopped", s)
	}

	f.exec(`UPDATE replicas SET stop_sent_at = NOW() - INTERVAL '1 minute' WHERE id = $1`, rep)
	if err := f.s.sendStops(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s := f.replicaState(rep); s != "stopped" {
		t.Fatalf("replica is %s after the grace period, want stopped", s)
	}
	if s := f.str(`SELECT status FROM gpus WHERE id = $1`, gpuID); s != "available" {
		t.Fatalf("GPU is %s after the stop, want available", s)
	}
}

func TestSilentHostGoesOfflineAndItsReplicasFail(t *testing.T) {
	f := setup(t)
	silent, silentGPU := f.host()
	alive, aliveGPU := f.host()
	dep := f.deployment()
	lost := f.replica(dep, silent, silentGPU)
	kept := f.replica(dep, alive, aliveGPU)
	f.exec(`UPDATE replicas SET state = 'serving' WHERE deployment_id = $1`, dep)
	f.exec(`UPDATE deployments SET state = 'serving', min_replicas = 2, max_replicas = 2 WHERE id = $1`, dep)
	f.exec(`UPDATE hosts SET last_heartbeat_at = NOW() - INTERVAL '1 minute' WHERE id = $1`, silent)
	st := f.connect(silent)
	ch, _ := f.s.sessions.get(silent).open("req-1")

	if err := f.s.sweepOffline(f.ctx, 15*time.Second); err != nil {
		t.Fatal(err)
	}

	if s := f.str(`SELECT status FROM hosts WHERE id = $1`, silent); s != "offline" {
		t.Fatalf("silent host is %s, want offline", s)
	}
	if s := f.str(`SELECT status FROM hosts WHERE id = $1`, alive); s != "active" {
		t.Fatalf("a host that is heartbeating was marked %s", s)
	}
	if s := f.replicaState(lost); s != "failed" {
		t.Fatalf("replica on the offline host is %s, want failed", s)
	}
	if s := f.replicaState(kept); s != "serving" {
		t.Fatalf("replica on the live host is %s, want serving", s)
	}
	if s := f.str(`SELECT status FROM gpus WHERE id = $1`, silentGPU); s != "available" {
		t.Fatalf("offline host's GPU is %s, want released", s)
	}
	if s := f.str(`SELECT state FROM deployments WHERE id = $1`, dep); s != "degraded" {
		t.Fatalf("deployment is %s with one of two replicas left, want degraded", s)
	}
	// A request in flight on the dead host is failed so the gateway can retry.
	select {
	case c := <-ch:
		if !c.GetDone() || c.GetError() == "" {
			t.Fatalf("in-flight request got %+v, want a terminal error", c)
		}
	default:
		t.Fatal("an in-flight request on the offline host was left hanging")
	}
	_ = st

	// A heartbeat brings the host back.
	var last time.Time
	f.s.onHeartbeat(f.ctx, silent, &agentv1.Heartbeat{RuntimeHealthy: true}, &last)
	if s := f.str(`SELECT status FROM hosts WHERE id = $1`, silent); s != "active" {
		t.Fatalf("host is %s after heartbeating again, want active", s)
	}
}

func TestReconnectResendsTheJob(t *testing.T) {
	f := setup(t)
	hostID, gpuID := f.host()
	dep := f.deployment()
	rep := f.replica(dep, hostID, gpuID)
	f.exec(`UPDATE replicas SET state = 'serving', dispatched_at = NOW() WHERE id = $1`, rep)
	f.exec(`UPDATE deployments SET state = 'serving' WHERE id = $1`, dep)

	if err := f.s.redispatchOnReconnect(f.ctx, hostID); err != nil {
		t.Fatal(err)
	}
	st := f.connect(hostID)
	if err := f.s.dispatchPending(f.ctx); err != nil {
		t.Fatal(err)
	}

	if msgs := st.messages(); len(msgs) != 1 || msgs[0].GetManifest().GetReplicaId() != rep {
		t.Fatal("a restarted agent was not sent its replica's manifest again")
	}
	if s := f.str(`SELECT status FROM gpus WHERE id = $1`, gpuID); s != "reserved" {
		t.Fatalf("GPU is %s during the re-dispatch, want it kept reserved", s)
	}
	if s := f.str(`SELECT state FROM deployments WHERE id = $1`, dep); s != "degraded" {
		t.Fatalf("deployment is %s while its only replica reloads, want degraded", s)
	}
}

// ─── Session ─────────────────────────────────────────────────

func TestSessionRegistersHeartbeatsAndCleansUp(t *testing.T) {
	f := setup(t)
	st := newStream()
	done := make(chan error, 1)
	st.in <- &agentv1.AgentMessage{Payload: &agentv1.AgentMessage_Register{Register: f.register("t3", "sha256:session", 8)}}
	go func() { done <- f.s.Session(st) }()

	var resp *agentv1.RegisterResponse
	select {
	case m := <-st.out:
		resp = m.GetRegisterResponse()
	case <-time.After(10 * time.Second):
		t.Fatal("no RegisterResponse")
	}
	if !resp.GetAccepted() || resp.GetHostCredential() == "" || len(resp.GetManifestPublicKey()) == 0 {
		t.Fatalf("RegisterResponse = %+v, want accepted with a credential and the manifest key", resp)
	}
	hostID := resp.GetHostId()

	f.exec(`UPDATE hosts SET last_heartbeat_at = NOW() - INTERVAL '1 hour', runtime_healthy = FALSE WHERE id = $1`, hostID)
	st.in <- &agentv1.AgentMessage{Payload: &agentv1.AgentMessage_Heartbeat{Heartbeat: &agentv1.Heartbeat{RuntimeHealthy: true, CachedModels: []string{"gemma2:2b"}}}}
	deadline := time.Now().Add(10 * time.Second)
	for f.str(`SELECT (runtime_healthy AND last_heartbeat_at > NOW() - INTERVAL '1 minute')::TEXT FROM hosts WHERE id = $1`, hostID) != "true" {
		if time.Now().After(deadline) {
			t.Fatal("the heartbeat was not recorded")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if f.s.sessions.get(hostID) == nil {
		t.Fatal("the connected host has no session")
	}

	close(st.in)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Session returned %v on a clean close", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Session did not return after the agent closed the stream")
	}
	if f.s.sessions.get(hostID) != nil {
		t.Fatal("the session outlived its stream")
	}
}

func TestSessionRejectsWithoutCredentials(t *testing.T) {
	f := setup(t)
	st := newStream()
	st.in <- &agentv1.AgentMessage{Payload: &agentv1.AgentMessage_Register{Register: &agentv1.RegisterRequest{Gpus: gpu(8)}}}
	err := f.s.Session(st)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Session returned %v, want Unauthenticated", err)
	}
	msgs := st.messages()
	if len(msgs) != 1 || msgs[0].GetRegisterResponse().GetAccepted() || msgs[0].GetRegisterResponse().GetRejectionReason() == "" {
		t.Fatal("a rejected agent must be told why")
	}
}

// ─── Inference tunnel ────────────────────────────────────────

func (f *fixture) infer(replicaID string, stream bool) *http.Request {
	body, _ := json.Marshal(inferRequest{
		RequestID: "req-" + replicaID, ReplicaID: replicaID, Path: "/v1/chat/completions",
		Body: json.RawMessage(`{"messages":[]}`), Stream: stream,
	})
	return httptest.NewRequest("POST", "/internal/v1/infer", bytes.NewReader(body)).WithContext(f.ctx)
}

func TestTunnelStreamsTheAnswerAndReportsUsage(t *testing.T) {
	f := setup(t)
	hostID, gpuID := f.host()
	rep := f.replica(f.deployment(), hostID, gpuID)
	f.exec(`UPDATE replicas SET state = 'serving' WHERE id = $1`, rep)
	st := f.connect(hostID)
	sess := f.s.sessions.get(hostID)

	// The agent: answer the request it is sent in two chunks.
	go func() {
		m := <-st.out
		id := m.GetInferenceRequest().GetRequestId()
		sess.deliver(&agentv1.InferenceChunk{RequestId: id, Data: []byte("data: hel\n\n"), StatusCode: 200}, f.ctx.Done())
		sess.deliver(&agentv1.InferenceChunk{RequestId: id, Data: []byte("data: lo\n\n"), Done: true, PromptTokens: 7, CompletionTokens: 2}, f.ctx.Done())
	}()

	rec := httptest.NewRecorder()
	f.s.handleInfer(rec, f.infer(rep, true))
	res := rec.Result()

	if res.StatusCode != 200 || rec.Body.String() != "data: hel\n\ndata: lo\n\n" {
		t.Fatalf("status %d body %q", res.StatusCode, rec.Body.String())
	}
	if res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type = %q, want text/event-stream", res.Header.Get("Content-Type"))
	}
	if p, c := res.Trailer.Get(TrailerPromptTokens), res.Trailer.Get(TrailerCompletionTokens); p != "7" || c != "2" {
		t.Fatalf("usage trailers = %q/%q, want 7/2: usage records depend on them", p, c)
	}
	sess.mu.Lock()
	left := len(sess.inflight)
	sess.mu.Unlock()
	if left != 0 {
		t.Fatal("the finished request is still registered on the session")
	}
}

func TestTunnelRefusesBeforeTheFirstByteSoTheGatewayCanRetry(t *testing.T) {
	f := setup(t)
	hostID, gpuID := f.host()
	rep := f.replica(f.deployment(), hostID, gpuID)

	call := func(req *http.Request) int {
		rec := httptest.NewRecorder()
		f.s.handleInfer(rec, req)
		return rec.Code
	}

	if code := call(f.infer(rep, false)); code != http.StatusServiceUnavailable {
		t.Fatalf("pending replica: status %d, want 503", code)
	}
	f.exec(`UPDATE replicas SET state = 'serving' WHERE id = $1`, rep)
	if code := call(f.infer(rep, false)); code != http.StatusServiceUnavailable {
		t.Fatalf("disconnected host: status %d, want 503", code)
	}
	if code := call(f.infer("00000000-0000-0000-0000-000000000000", false)); code != http.StatusNotFound {
		t.Fatalf("unknown replica: status %d, want 404", code)
	}
	bad, _ := json.Marshal(inferRequest{RequestID: "x", ReplicaID: rep, Path: "/api/pull"})
	if code := call(httptest.NewRequest("POST", "/internal/v1/infer", bytes.NewReader(bad))); code != http.StatusBadRequest {
		t.Fatalf("a path outside the inference API returned %d, want 400", code)
	}

	// The runtime answers with an error and no body: the status is passed on.
	st := f.connect(hostID)
	sess := f.s.sessions.get(hostID)
	go func() {
		m := <-st.out
		sess.deliver(&agentv1.InferenceChunk{RequestId: m.GetInferenceRequest().GetRequestId(), Done: true, StatusCode: 503, Error: "model not loaded"}, f.ctx.Done())
	}()
	if code := call(f.infer(rep, false)); code != http.StatusServiceUnavailable {
		t.Fatalf("runtime error: status %d, want the runtime's 503", code)
	}

	// The host drops mid-request: the caller gets a gateway error, not a hang.
	go func() {
		<-st.out
		sess.close()
	}()
	if code := call(f.infer(rep, false)); code != http.StatusBadGateway {
		t.Fatalf("host disconnect: status %d, want 502", code)
	}
}
