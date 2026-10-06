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
	// failSends makes every Send fail, as on a stream whose peer has gone.
	failSends bool

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
	if f.failSends {
		return status.Error(codes.Unavailable, "transport is closing")
	}
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

// The credential says which host this is; the fingerprint only describes the
// machine, and descriptions change. A driver update must never lock a host out.
func TestAChangedFingerprintIsAcceptedAndRecorded(t *testing.T) {
	f := setup(t)
	en, _, _ := f.s.enrol(f.ctx, f.register("t3", "sha256:before-the-driver-update", 8))
	back := func(fp string) *agentv1.RegisterRequest {
		return &agentv1.RegisterRequest{HostCredential: en.credential, HardwareFingerprint: fp, Gpus: gpu(8), Runtime: "ollama"}
	}

	again, code, reason := f.s.resume(f.ctx, back("sha256:after-the-driver-update"))
	if code != codes.OK || again.hostID != en.hostID {
		t.Fatalf("a host whose fingerprint changed was refused or became another host: %s %q", code, reason)
	}
	if fp := f.str(`SELECT hw_fingerprint FROM hosts WHERE id = $1`, en.hostID); fp != "sha256:after-the-driver-update" {
		t.Fatalf("the record still says %s", fp)
	}
	var hosts int
	f.scan(&hosts, `SELECT COUNT(*) FROM hosts`)
	if hosts != 1 {
		t.Fatalf("%d hosts after a fingerprint change, want the one", hosts)
	}
	// An agent that sends no fingerprint leaves the record alone.
	if _, code, _ := f.s.resume(f.ctx, back("")); code != codes.OK {
		t.Fatalf("resume without a fingerprint: %s", code)
	}
	if fp := f.str(`SELECT hw_fingerprint FROM hosts WHERE id = $1`, en.hostID); fp != "sha256:after-the-driver-update" {
		t.Fatalf("an empty fingerprint overwrote the record: %s", fp)
	}

	// A fingerprint another host already has (two machines cloned from one
	// image share a machine id) is not adopted, and does not lock this host
	// out either: the credential is what identifies it.
	other, _, _ := f.s.enrol(f.ctx, f.register("t3", "sha256:another-machine", 8))
	same, code, reason := f.s.resume(f.ctx, back("sha256:another-machine"))
	if code != codes.OK || same.hostID != en.hostID {
		t.Fatalf("a host reporting a fingerprint another host has = %s %q, want it kept as itself", code, reason)
	}
	if fp := f.str(`SELECT hw_fingerprint FROM hosts WHERE id = $1`, en.hostID); fp != "sha256:after-the-driver-update" {
		t.Fatalf("the record took a fingerprint that belongs to another host: %s", fp)
	}
	if fp := f.str(`SELECT hw_fingerprint FROM hosts WHERE id = $1`, other.hostID); fp != "sha256:another-machine" {
		t.Fatalf("the other machine's record was changed: %s", fp)
	}
}

// A machine enrolled by an older agent, then enrolled again by a newer one that
// computes its identity differently, stays one machine.
func TestReEnrolmentRecognisesAnOlderFingerprint(t *testing.T) {
	f := setup(t)
	first, code, reason := f.s.enrol(f.ctx, f.register("t3", "sha256:as-the-old-agent-saw-it", 8))
	if code != codes.OK {
		t.Fatal(reason)
	}

	upgraded := f.register("t3", "sha256:as-the-new-agent-sees-it", 8)
	upgraded.LegacyFingerprints = []string{"sha256:something-else", "sha256:as-the-old-agent-saw-it"}
	again, code, reason := f.s.enrol(f.ctx, upgraded)
	if code != codes.OK {
		t.Fatalf("re-enrolment with a legacy fingerprint: %s %q", code, reason)
	}
	if again.hostID != first.hostID {
		t.Fatal("the same machine became a second host after an agent upgrade")
	}
	if fp := f.str(`SELECT hw_fingerprint FROM hosts WHERE id = $1`, first.hostID); fp != "sha256:as-the-new-agent-sees-it" {
		t.Fatalf("the record was not moved to the current fingerprint: %s", fp)
	}
	var hosts int
	f.scan(&hosts, `SELECT COUNT(*) FROM hosts`)
	if hosts != 1 {
		t.Fatalf("%d hosts, want 1", hosts)
	}
	if _, code, _ := f.s.resume(f.ctx, &agentv1.RegisterRequest{HostCredential: first.credential, Gpus: gpu(8)}); code != codes.Unauthenticated {
		t.Fatalf("the credential from before re-enrolment still works (code %s)", code)
	}

	// Someone else's host that has claimed this machine's fingerprint (a
	// fingerprint is only a string the agent sends) does not get in the way of
	// the owner enrolling it again, and is not touched by it.
	owner := f.user
	f.scan(&f.user, `INSERT INTO users (email, name) VALUES ('squatter-' || gen_random_uuid() || '@example.com', 'Squatter') RETURNING id`)
	squat, code, reason := f.s.enrol(f.ctx, f.register("t3", "sha256:squatter-own", 8))
	if code != codes.OK {
		t.Fatal(reason)
	}
	if _, code, _ := f.s.resume(f.ctx, &agentv1.RegisterRequest{HostCredential: squat.credential, HardwareFingerprint: "sha256:what-the-owner-will-be-next", Gpus: gpu(8)}); code != codes.OK {
		t.Fatal("resume")
	}
	f.user = owner
	next := f.register("t3", "sha256:what-the-owner-will-be-next", 8)
	next.LegacyFingerprints = []string{"sha256:as-the-new-agent-sees-it"}
	mine, code, reason := f.s.enrol(f.ctx, next)
	if code != codes.OK || mine.hostID != first.hostID {
		t.Fatalf("the owner could not enrol their own machine again once another host claimed its fingerprint: %s %q", code, reason)
	}
	if u := f.str(`SELECT user_id::TEXT FROM hosts WHERE id = $1`, squat.hostID); u == owner {
		t.Fatal("enrolling took over another account's host")
	}
	if fp := f.str(`SELECT hw_fingerprint FROM hosts WHERE id = $1`, first.hostID); fp != "sha256:as-the-new-agent-sees-it" {
		t.Fatalf("the owner's record changed fingerprint to one another host holds: %s", fp)
	}

	// A legacy fingerprint does not let another account take the machine.
	f.scan(&f.user, `INSERT INTO users (email, name) VALUES ('other-' || gen_random_uuid() || '@example.com', 'Other') RETURNING id`)
	theirs := f.register("t3", "sha256:a-third-description", 8)
	theirs.LegacyFingerprints = []string{"sha256:as-the-new-agent-sees-it"}
	if _, code, _ := f.s.enrol(f.ctx, theirs); code != codes.PermissionDenied {
		t.Fatalf("another account enrolled the machine through a legacy fingerprint (code %s)", code)
	}
	// And a banned machine is still banned under its new description.
	f.exec(`UPDATE hosts SET status = 'banned' WHERE id = $1`, first.hostID)
	f.user = f.str(`SELECT user_id::TEXT FROM hosts WHERE id = $1`, first.hostID)
	dodge := f.register("t3", "sha256:a-fourth-description", 8)
	dodge.LegacyFingerprints = []string{"sha256:as-the-new-agent-sees-it"}
	if _, code, _ := f.s.enrol(f.ctx, dodge); code != codes.PermissionDenied {
		t.Fatalf("a banned machine re-enrolled under a new fingerprint (code %s)", code)
	}
}

// GPUs are attributes of a machine, brought up to date every time it connects.
func TestGPUsFollowWhatTheMachineReports(t *testing.T) {
	f := setup(t)
	card := func(uuid, driver string, vram int32) *agentv1.GpuInfo {
		return &agentv1.GpuInfo{Model: "NVIDIA GeForce RTX 4090", VramGb: vram, Uuid: uuid, DriverVersion: driver}
	}
	reg := f.register("t3", "sha256:two-cards", 24)
	reg.Gpus = []*agentv1.GpuInfo{card("GPU-a", "550.54", 24), card("GPU-b", "550.54", 24)}
	en, code, reason := f.s.enrol(f.ctx, reg)
	if code != codes.OK {
		t.Fatal(reason)
	}
	status := func(uuid string) string {
		return f.str(`SELECT status FROM gpus WHERE host_id = $1 AND uuid = $2`, en.hostID, uuid)
	}
	back := func(gpus ...*agentv1.GpuInfo) {
		t.Helper()
		if _, code, reason := f.s.resume(f.ctx, &agentv1.RegisterRequest{HostCredential: en.credential, HardwareFingerprint: "sha256:two-cards", Gpus: gpus, Runtime: "ollama"}); code != codes.OK {
			t.Fatalf("resume: %s %q", code, reason)
		}
	}

	// Replicas on both cards, one of them serving.
	gpuA := f.str(`SELECT id FROM gpus WHERE host_id = $1 AND uuid = 'GPU-a'`, en.hostID)
	gpuB := f.str(`SELECT id FROM gpus WHERE host_id = $1 AND uuid = 'GPU-b'`, en.hostID)
	dep := f.deployment()
	f.exec(`UPDATE deployments SET min_replicas = 2, max_replicas = 2, state = 'serving' WHERE id = $1`, dep)
	onA := f.replica(dep, en.hostID, gpuA)
	onB := f.replica(dep, en.hostID, gpuB)
	f.exec(`UPDATE replicas SET state = 'serving' WHERE deployment_id = $1`, dep)

	// A driver update: same cards, new driver. Nothing is disturbed.
	back(card("GPU-a", "560.35", 24), card("GPU-b", "560.35", 24))
	if d := f.str(`SELECT driver_version FROM gpus WHERE id = $1`, gpuA); d != "560.35" {
		t.Fatalf("driver version on record is %s after an update", d)
	}
	if status("GPU-a") != "reserved" || status("GPU-b") != "reserved" || f.replicaState(onA) != "serving" || f.replicaState(onB) != "serving" {
		t.Fatalf("a driver update disturbed the host: GPUs %s/%s, replicas %s/%s", status("GPU-a"), status("GPU-b"), f.replicaState(onA), f.replicaState(onB))
	}

	// Card B is taken out; card C is put in.
	back(card("GPU-a", "560.35", 24), card("GPU-c", "560.35", 16))
	if s := status("GPU-b"); s != "unavailable" {
		t.Fatalf("a card that is gone is %s, want unavailable", s)
	}
	if s := f.replicaState(onB); s != "failed" {
		t.Fatalf("the replica on the card that is gone is %s, want failed so it is placed elsewhere", s)
	}
	if owner := f.str(`SELECT COALESCE(replica_id::TEXT, '') FROM gpus WHERE id = $1`, gpuB); owner != "" {
		t.Fatal("the missing card is still held by its replica")
	}
	if status("GPU-a") != "reserved" || f.replicaState(onA) != "serving" {
		t.Fatalf("the card that stayed was disturbed: %s, replica %s", status("GPU-a"), f.replicaState(onA))
	}
	if s := status("GPU-c"); s != "available" {
		t.Fatalf("the new card is %s, want available", s)
	}
	if s := f.str(`SELECT state FROM deployments WHERE id = $1`, dep); s != "degraded" {
		t.Fatalf("deployment is %s with one of two replicas left, want degraded", s)
	}

	// Card B comes back: usable again, and the old replica stays failed.
	back(card("GPU-a", "560.35", 24), card("GPU-b", "560.35", 24), card("GPU-c", "560.35", 16))
	if s := status("GPU-b"); s != "available" {
		t.Fatalf("a card that came back is %s, want available", s)
	}
	if s := f.replicaState(onB); s != "failed" {
		t.Fatalf("a failed replica came back to life: %s", s)
	}
	var cards int
	f.scan(&cards, `SELECT COUNT(*) FROM gpus WHERE host_id = $1`, en.hostID)
	if cards != 3 {
		t.Fatalf("%d GPU rows for three cards", cards)
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
	// If that host was only away and still holds the model, it is told to
	// stop the moment it reconnects.
	if orphans, err := f.s.reconcileOnRegister(f.ctx, offlineHost, reporting(gone)); err != nil || len(orphans) != 1 || orphans[0] != gone {
		t.Fatalf("a returning host still holding a stopped replica: orphans = %v, %v; want it told to stop", orphans, err)
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

// A host that falls silent is marked offline at once, but a replica it was
// serving gets a minute for the host to come back before it is replaced: the
// model is almost certainly still loaded on a machine whose Wi-Fi blinked.
func TestSilentHostGoesOfflineAndItsServingReplicasWait(t *testing.T) {
	f := setup(t)
	silent, silentGPU := f.host()
	alive, aliveGPU := f.host()
	dep := f.deployment()
	lost := f.replica(dep, silent, silentGPU)
	kept := f.replica(dep, alive, aliveGPU)
	f.exec(`UPDATE replicas SET state = 'serving' WHERE deployment_id = $1`, dep)
	f.exec(`UPDATE deployments SET state = 'serving', min_replicas = 2, max_replicas = 2 WHERE id = $1`, dep)
	// A second job on the silent host had not finished starting.
	var startingGPU string
	f.scan(&startingGPU, `INSERT INTO gpus (host_id, model, vram_gb, uuid) VALUES ($1, 'Test GPU', 8, 'GPU-' || gen_random_uuid()) RETURNING id`, silent)
	dep2 := f.deployment()
	starting := f.replica(dep2, silent, startingGPU)
	f.exec(`UPDATE replicas SET state = 'loading' WHERE id = $1`, starting)
	f.exec(`UPDATE hosts SET last_heartbeat_at = NOW() - INTERVAL '1 minute' WHERE id = $1`, silent)
	f.connect(silent)
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
	if s := f.replicaState(lost); s != "degraded" {
		t.Fatalf("serving replica on the offline host is %s, want degraded while the host may return", s)
	}
	if s := f.str(`SELECT status FROM gpus WHERE id = $1`, silentGPU); s != "reserved" {
		t.Fatalf("the waiting replica's GPU is %s, want it kept reserved", s)
	}
	if s := f.replicaState(starting); s != "failed" {
		t.Fatalf("a replica that was still starting on the offline host is %s, want failed so it is placed elsewhere", s)
	}
	if s := f.replicaState(kept); s != "serving" {
		t.Fatalf("replica on the live host is %s, want serving", s)
	}
	if s := f.str(`SELECT state FROM deployments WHERE id = $1`, dep); s != "degraded" {
		t.Fatalf("deployment is %s with one of two replicas reachable, want degraded", s)
	}
	// The dead connection is gone, not left for jobs to be sent into.
	if f.s.sessions.get(silent) != nil {
		t.Fatal("the offline host still has a session")
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

	// Inside the minute nothing more happens.
	if err := f.s.failUnreturned(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s := f.replicaState(lost); s != "degraded" {
		t.Fatalf("the replica was given up after no time at all: %s", s)
	}
	// After it, the replica is given up and its GPU released.
	f.exec(`UPDATE replicas SET updated_at = NOW() - INTERVAL '2 minutes' WHERE id = $1`, lost)
	if err := f.s.failUnreturned(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s := f.replicaState(lost); s != "failed" {
		t.Fatalf("replica is %s a minute after its host vanished, want failed", s)
	}
	if s := f.str(`SELECT status FROM gpus WHERE id = $1`, silentGPU); s != "available" {
		t.Fatalf("the given-up replica's GPU is %s, want released", s)
	}

	// A heartbeat brings the host back.
	var last time.Time
	f.s.onHeartbeat(f.ctx, silent, &agentv1.Heartbeat{RuntimeHealthy: true}, &last)
	if s := f.str(`SELECT status FROM hosts WHERE id = $1`, silent); s != "active" {
		t.Fatalf("host is %s after heartbeating again, want active", s)
	}
}

// reporting is what an agent that lists its replicas sends when it registers.
func reporting(replicas ...string) *agentv1.RegisterRequest {
	reg := &agentv1.RegisterRequest{Capabilities: []string{capReplicaReport}}
	for _, id := range replicas {
		reg.Replicas = append(reg.Replicas, &agentv1.HeldReplica{ReplicaId: id, RuntimeModel: "gemma2:2b"})
	}
	return reg
}

// The whole point of the reconnect handshake: a blip changes nothing.
func TestReconnectLeavesAnAgreedReplicaAlone(t *testing.T) {
	f := setup(t)
	hostID, gpuID := f.host()
	dep := f.deployment()
	rep := f.replica(dep, hostID, gpuID)
	f.exec(`UPDATE replicas SET state = 'serving', healthy = TRUE, dispatched_at = NOW() - INTERVAL '1 hour',
	        started_at = NOW() - INTERVAL '1 hour', updated_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, rep)
	f.exec(`UPDATE deployments SET state = 'serving' WHERE id = $1`, dep)
	before := f.str(`SELECT updated_at::TEXT || '|' || dispatched_at::TEXT || '|' || started_at::TEXT FROM replicas WHERE id = $1`, rep)
	var events int
	f.scan(&events, `SELECT COUNT(*) FROM deployment_events WHERE deployment_id = $1`, dep)

	orphans, err := f.s.reconcileOnRegister(f.ctx, hostID, reporting(rep))
	if err != nil || len(orphans) != 0 {
		t.Fatalf("reconcile = %v, %v", orphans, err)
	}
	st := f.connect(hostID)
	if err := f.s.dispatchPending(f.ctx); err != nil {
		t.Fatal(err)
	}

	if len(st.messages()) != 0 {
		t.Fatalf("the host was sent %d messages for a replica it is already serving", len(st.messages()))
	}
	if after := f.str(`SELECT updated_at::TEXT || '|' || dispatched_at::TEXT || '|' || started_at::TEXT FROM replicas WHERE id = $1`, rep); after != before {
		t.Fatalf("the replica row was touched: %s -> %s", before, after)
	}
	if s := f.replicaState(rep); s != "serving" {
		t.Fatalf("replica is %s, want serving", s)
	}
	if s := f.str(`SELECT state FROM deployments WHERE id = $1`, dep); s != "serving" {
		t.Fatalf("deployment is %s across a reconnect, want serving throughout", s)
	}
	var after int
	f.scan(&after, `SELECT COUNT(*) FROM deployment_events WHERE deployment_id = $1`, dep)
	if after != events {
		t.Fatalf("%d events were logged for a reconnect that changed nothing", after-events)
	}
}

func TestReconnectReconcilesEveryCase(t *testing.T) {
	f := setup(t)
	hostID, _ := f.host()
	other, otherGPU := f.host()
	place := func(state string) (rep, dep string) {
		var g string
		f.scan(&g, `INSERT INTO gpus (host_id, model, vram_gb, uuid) VALUES ($1, 'Test GPU', 8, 'GPU-' || gen_random_uuid()) RETURNING id`, hostID)
		dep = f.deployment()
		rep = f.replica(dep, hostID, g)
		f.exec(`UPDATE replicas SET state = $2, dispatched_at = NOW(), stop_sent_at = CASE WHEN $2 = 'stopping' THEN NOW() END WHERE id = $1`, rep, state)
		f.exec(`UPDATE deployments SET state = 'serving' WHERE id = $1`, dep)
		return rep, dep
	}
	forgotten, forgottenDep := place("serving") // the agent restarted and lost it
	waited, _ := place("degraded")              // the host was unreachable for a while
	unheard, _ := place("warming")              // it finished starting while we could not hear it
	stopping, _ := place("stopping")            // a stop was sent to the old connection
	stopped, _ := place("stopped")              // stopped while the host was away
	failed, _ := place("failed")                // given up and replaced while the host was away
	halfway, _ := place("pulling")              // was mid-download; the agent starts over
	elsewhere := f.replica(f.deployment(), other, otherGPU)
	f.exec(`UPDATE replicas SET state = 'serving' WHERE id = $1`, elsewhere)
	const unknown = "00000000-0000-4000-8000-000000000001"

	orphans, err := f.s.reconcileOnRegister(f.ctx, hostID, reporting(waited, unheard, stopping, stopped, failed, elsewhere, unknown))
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ name, id, want string }{
		{"lost by a restarted agent", forgotten, "pending"},
		{"kept through an outage", waited, "serving"},
		{"finished starting unheard", unheard, "serving"},
		{"being stopped", stopping, "stopping"},
		{"stopped meanwhile", stopped, "stopped"},
		{"given up meanwhile", failed, "failed"},
		{"interrupted mid-download", halfway, "pending"},
		{"another host's", elsewhere, "serving"},
	} {
		if got := f.replicaState(c.id); got != c.want {
			t.Errorf("replica %s: %s, want %s", c.name, got, c.want)
		}
	}
	for _, id := range []string{forgotten, halfway} {
		if f.str(`SELECT (dispatched_at IS NULL)::TEXT FROM replicas WHERE id = $1`, id) != "true" {
			t.Errorf("replica %s is pending but would not be sent again", id)
		}
	}
	if f.str(`SELECT (stop_sent_at IS NULL)::TEXT FROM replicas WHERE id = $1`, stopping) != "true" {
		t.Error("the stop would not be sent again on the new connection")
	}
	if s := f.str(`SELECT state FROM deployments WHERE id = $1`, forgottenDep); s != "degraded" {
		t.Errorf("deployment is %s while its only replica reloads, want degraded", s)
	}

	// What the agent holds but should not: told to stop, never adopted. That
	// includes the replica being stopped, which it is told directly as well.
	want := map[string]bool{stopping: true, stopped: true, failed: true, elsewhere: true, unknown: true}
	if len(orphans) != len(want) {
		t.Fatalf("orphans = %v, want %d of them", orphans, len(want))
	}
	for _, id := range orphans {
		if !want[id] {
			t.Errorf("replica %s was wrongly listed to be stopped", id)
		}
	}

	// And the two that are pending are sent again; nothing else is.
	st := f.connect(hostID)
	if err := f.s.dispatchPending(f.ctx); err != nil {
		t.Fatal(err)
	}
	resent := map[string]bool{}
	for _, m := range st.messages() {
		resent[m.GetManifest().GetReplicaId()] = true
	}
	if len(resent) != 2 || !resent[forgotten] || !resent[halfway] {
		t.Fatalf("manifests re-sent for %v, want exactly the two the agent does not have", resent)
	}
}

// An agent from before the handshake says nothing about what it holds, so it
// is sent everything again, as it always was.
func TestReconnectResendsEverythingToAnAgentThatDoesNotReport(t *testing.T) {
	f := setup(t)
	hostID, gpuID := f.host()
	dep := f.deployment()
	rep := f.replica(dep, hostID, gpuID)
	f.exec(`UPDATE replicas SET state = 'serving', dispatched_at = NOW() WHERE id = $1`, rep)
	f.exec(`UPDATE deployments SET state = 'serving' WHERE id = $1`, dep)

	// Even if it lists replicas, without the capability they are not trusted.
	old := &agentv1.RegisterRequest{Replicas: []*agentv1.HeldReplica{{ReplicaId: rep}}}
	orphans, err := f.s.reconcileOnRegister(f.ctx, hostID, old)
	if err != nil || len(orphans) != 0 {
		t.Fatalf("reconcile = %v, %v", orphans, err)
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

// After a start, or a stall (the computer slept, the database was away), every
// heartbeat on record is stale through no fault of the hosts. Nobody is
// declared offline until they have had time to reconnect.
func TestHostsGetAGracePeriodAfterAStartOrAStall(t *testing.T) {
	f := setup(t)
	hostID, gpuID := f.host()
	dep := f.deployment()
	rep := f.replica(dep, hostID, gpuID)
	f.exec(`UPDATE replicas SET state = 'serving' WHERE id = $1`, rep)
	f.exec(`UPDATE deployments SET state = 'serving' WHERE id = $1`, dep)
	silent := func() {
		f.exec(`UPDATE hosts SET last_heartbeat_at = NOW() - INTERVAL '30 minutes', status = 'active' WHERE id = $1`, hostID)
	}
	status := func() string { return f.str(`SELECT status FROM hosts WHERE id = $1`, hostID) }

	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	f.s.clock = func() time.Time { return now }
	f.s.lastTick, f.s.graceUntil = time.Time{}, time.Time{} // a fresh process
	pass := func(seconds int) {
		for range seconds {
			now = now.Add(time.Second)
			f.s.tick(f.ctx, 15*time.Second)
		}
	}

	// Just started: the host has not been heard from for half an hour (the
	// platform was down), and that is not held against it.
	silent()
	pass(int(livenessGrace.Seconds()) - 2)
	if status() != "active" || f.replicaState(rep) != "serving" {
		t.Fatalf("inside the grace period after a start: host %s, replica %s; want both untouched", status(), f.replicaState(rep))
	}
	// Still silent once the period is over: now it counts.
	pass(5)
	if status() != "offline" || f.replicaState(rep) != "degraded" {
		t.Fatalf("after the grace period: host %s, replica %s; want offline and degraded", status(), f.replicaState(rep))
	}

	// The computer sleeps for an hour. On waking, the same patience again.
	f.exec(`UPDATE replicas SET state = 'serving' WHERE id = $1`, rep)
	silent()
	now = now.Add(time.Hour)
	pass(int(livenessGrace.Seconds()) - 2)
	if status() != "active" || f.replicaState(rep) != "serving" {
		t.Fatalf("inside the grace period after a stall: host %s, replica %s; want both untouched", status(), f.replicaState(rep))
	}
	// A host that does reconnect in that time is never marked at all.
	f.exec(`UPDATE hosts SET last_heartbeat_at = NOW() WHERE id = $1`, hostID)
	pass(5)
	if status() != "active" || f.replicaState(rep) != "serving" {
		t.Fatalf("a host that came back inside the grace period: host %s, replica %s", status(), f.replicaState(rep))
	}

	// Ordinary running: no grace, silence is noticed on the next pass.
	silent()
	pass(1)
	if status() != "offline" {
		t.Fatalf("a host that falls silent in ordinary running is %s, want offline at once", status())
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

// start runs a Session for a newly enrolling agent and returns once it is
// registered.
func (f *fixture) start(t *testing.T, reg *agentv1.RegisterRequest) (st *fakeStream, hostID, credential string, done chan error) {
	t.Helper()
	st = newStream()
	t.Cleanup(st.cancel)
	done = make(chan error, 1)
	st.in <- &agentv1.AgentMessage{Payload: &agentv1.AgentMessage_Register{Register: reg}}
	go func() { done <- f.s.Session(st) }()
	var resp *agentv1.RegisterResponse
	select {
	case m := <-st.out:
		resp = m.GetRegisterResponse()
		if !resp.GetAccepted() {
			t.Fatalf("registration refused: %s", resp.GetRejectionReason())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no RegisterResponse")
	}
	// The answer is sent before the session is installed; wait for this
	// stream's session, not whichever one the host had before.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if sess := f.s.sessions.get(resp.GetHostId()); sess != nil && sess.stream == st {
			return st, resp.GetHostId(), resp.GetHostCredential(), done
		}
		if time.Now().After(deadline) {
			t.Fatal("the session was never installed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func ended(t *testing.T, done chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("the session's handler is still running after %s", what)
		return nil
	}
}

// Closing a session from the platform's side has to end the stream. A handler
// left blocked on a closed session is a host that looks connected and can
// never be sent anything again.
func TestClosingASessionEndsItsStream(t *testing.T) {
	f := setup(t)
	_, hostID, credential, done := f.start(t, f.register("t3", "sha256:closing", 8))

	// The host is declared offline.
	f.s.sessions.drop(hostID)
	if err := ended(t, done, "the host was dropped"); status.Code(err) != codes.Unavailable {
		t.Fatalf("a dropped session ended with %v, want Unavailable so the agent reconnects", err)
	}
	if f.s.sessions.get(hostID) != nil {
		t.Fatal("a dropped host still has a session")
	}

	// The agent reconnects twice without the first connection ever closing
	// (it is behind a NAT that forgot it). The newer one replaces the older.
	back := func() *agentv1.RegisterRequest {
		r := reporting()
		r.HostCredential, r.HardwareFingerprint, r.Gpus, r.Runtime = credential, "sha256:closing", gpu(8), "ollama"
		return r
	}
	_, _, _, first := f.start(t, back())
	_, _, _, second := f.start(t, back())
	if err := ended(t, first, "a newer session replaced it"); status.Code(err) != codes.Unavailable {
		t.Fatalf("a replaced session ended with %v, want Unavailable", err)
	}
	select {
	case err := <-second:
		t.Fatalf("the newer session ended too: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if sess := f.s.sessions.get(hostID); sess == nil {
		t.Fatal("the reconnected host has no session")
	} else if err := sess.send(&agentv1.CoordinatorMessage{}); err != nil {
		t.Fatalf("the current session cannot be sent to: %v", err)
	}
}

// An agent that vanishes (killed, or its network gone) must not leave a
// session behind: one that looks connected but can never be written to.
func TestAVanishedAgentLeavesNoSession(t *testing.T) {
	f := setup(t)
	// The stream's context ending and its Recv failing happen together, and
	// which the handler notices first is a coin toss: try it many times.
	for i := range 25 {
		st, hostID, _, done := f.start(t, f.register("t3", fmt.Sprintf("sha256:vanish-%d", i), 8))
		st.cancel()
		if err := ended(t, done, "the agent vanished"); err != nil {
			t.Fatalf("round %d: Session returned %v for an agent that went away", i, err)
		}
		if f.s.sessions.get(hostID) != nil {
			t.Fatalf("round %d: a vanished agent still has a session", i)
		}
	}
}

// A stream that cannot be written to is finished, whatever the registry says.
func TestAFailedSendDropsTheSession(t *testing.T) {
	f := setup(t)
	hostID, gpuID := f.host()
	dep := f.deployment()
	rep := f.replica(dep, hostID, gpuID)
	f.exec(`UPDATE replicas SET state = 'stopping' WHERE id = $1`, rep)
	st := f.connect(hostID)
	st.failSends = true

	if err := f.s.sendStops(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.s.sessions.get(hostID) != nil {
		t.Fatal("a session whose stream cannot be written to was kept")
	}
	if err := f.s.sendStops(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s := f.replicaState(rep); s != "stopped" {
		t.Fatalf("replica is %s after its host's stream broke, want stopped rather than stopping for ever", s)
	}
}

// A slow reader of an inference answer can hold up the handler that delivers
// it. Closing the session has to get through anyway.
func TestClosingASessionUnblocksADeliveryInProgress(t *testing.T) {
	st := newStream()
	t.Cleanup(st.cancel)
	sess := newSession("host", st)
	ch, _ := sess.open("req")
	for range cap(ch) {
		sess.deliver(&agentv1.InferenceChunk{RequestId: "req"}, st.ctx.Done())
	}
	blocked := make(chan struct{})
	go func() {
		sess.deliver(&agentv1.InferenceChunk{RequestId: "req"}, st.ctx.Done()) // the reader has stopped
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Fatal("delivery into a full channel did not wait")
	case <-time.After(100 * time.Millisecond):
	}
	sess.close()
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("a delivery in progress outlived the session; its handler could never end the stream")
	}
}

// A session fetched a moment ago may no longer be the host's current one.
func TestDroppingAStaleSessionLeavesTheNewOne(t *testing.T) {
	f := setup(t)
	hostID, _ := f.host()
	oldStream, newStream := f.connect(hostID), newStream()
	t.Cleanup(newStream.cancel)
	old := f.s.sessions.get(hostID)
	_ = oldStream
	fresh := newSession(hostID, newStream)
	f.s.sessions.put(fresh).close() // the agent reconnected

	f.s.sessions.dropIf(old)
	if f.s.sessions.get(hostID) != fresh {
		t.Fatal("dropping a stale session threw out the host's new one")
	}
	if err := fresh.send(&agentv1.CoordinatorMessage{}); err != nil {
		t.Fatalf("the new session was closed: %v", err)
	}
	f.s.sessions.dropIf(fresh)
	if f.s.sessions.get(hostID) != nil {
		t.Fatal("the current session was not dropped")
	}
}

// A replica must not be left degraded on a host that is back: nothing else
// would ever move it. It is sent again, and an agent that still has the model
// answers at once.
func TestADegradedReplicaOnAReturnedHostIsSentAgain(t *testing.T) {
	f := setup(t)
	hostID, gpuID := f.host()
	dep := f.deployment()
	rep := f.replica(dep, hostID, gpuID)
	f.exec(`UPDATE replicas SET state = 'degraded', dispatched_at = NOW() WHERE id = $1`, rep)
	f.exec(`UPDATE deployments SET state = 'degraded' WHERE id = $1`, dep)

	if err := f.s.failUnreturned(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s := f.replicaState(rep); s != "degraded" {
		t.Fatalf("a freshly degraded replica was touched: %s", s)
	}
	f.exec(`UPDATE replicas SET updated_at = NOW() - INTERVAL '2 minutes' WHERE id = $1`, rep)
	if err := f.s.failUnreturned(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s := f.replicaState(rep); s != "pending" {
		t.Fatalf("replica is %s on a host that is back, want pending so it is sent again", s)
	}
	st := f.connect(hostID)
	if err := f.s.dispatchPending(f.ctx); err != nil {
		t.Fatal(err)
	}
	if msgs := st.messages(); len(msgs) != 1 || msgs[0].GetManifest().GetReplicaId() != rep {
		t.Fatal("the replica was not sent to its host again")
	}
	if s := f.str(`SELECT status FROM gpus WHERE id = $1`, gpuID); s != "reserved" {
		t.Fatalf("its GPU is %s, want kept", s)
	}
}

// Shutting down ends every session, so agents move to the process that
// replaces this one instead of staying attached to it.
func TestShutdownEndsEverySession(t *testing.T) {
	f := setup(t)
	_, a, _, doneA := f.start(t, f.register("t3", "sha256:shutdown-a", 8))
	_, b, _, doneB := f.start(t, f.register("t3", "sha256:shutdown-b", 8))

	f.s.sessions.closeAll()

	for _, done := range []chan error{doneA, doneB} {
		if err := ended(t, done, "shutdown"); status.Code(err) != codes.Unavailable {
			t.Fatalf("a session ended with %v at shutdown, want Unavailable", err)
		}
	}
	if f.s.sessions.get(a) != nil || f.s.sessions.get(b) != nil || len(f.s.sessions.connected()) != 0 {
		t.Fatal("sessions remain after shutdown")
	}
}

// What the agent holds and should not is stopped as soon as it registers.
func TestSessionStopsWhatTheAgentShouldNotBeRunning(t *testing.T) {
	f := setup(t)
	_, hostID, credential, done := f.start(t, f.register("t3", "sha256:orphans", 8))
	f.s.sessions.drop(hostID)
	ended(t, done, "the host was dropped")

	gpuID := f.str(`SELECT id FROM gpus WHERE host_id = $1`, hostID)
	dep := f.deployment()
	kept := f.replica(dep, hostID, gpuID)
	f.exec(`UPDATE replicas SET state = 'serving' WHERE id = $1`, kept)
	gone := f.replica(f.deployment(), hostID, gpuID)
	f.exec(`UPDATE replicas SET state = 'failed' WHERE id = $1`, gone)

	reg := reporting(kept, gone)
	reg.HostCredential, reg.HardwareFingerprint, reg.Gpus, reg.Runtime = credential, "sha256:orphans", gpu(8), "ollama"
	st, _, _, _ := f.start(t, reg)

	deadline := time.Now().Add(5 * time.Second)
	for {
		var stops []string
		for _, m := range st.messages() {
			if sr := m.GetStopReplica(); sr != nil {
				stops = append(stops, sr.GetReplicaId())
			}
			if m.GetManifest() != nil {
				t.Fatalf("a manifest was sent for a replica the agent is already serving")
			}
		}
		if len(stops) == 1 && stops[0] == gone {
			break
		}
		if len(stops) > 1 || time.Now().After(deadline) {
			t.Fatalf("stops sent = %v, want exactly the replica the platform gave up on", stops)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s := f.replicaState(kept); s != "serving" {
		t.Fatalf("the replica both sides agree on is %s, want serving", s)
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
