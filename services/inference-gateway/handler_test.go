package main

import (
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

	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/testutil"
	"github.com/google/uuid"
)

// gemma-2-2b-it from the seed catalogue.
const smallModel = "550e8400-e29b-41d4-a716-446655440015"

var testDB *db.Client

func TestMain(m *testing.M) {
	client, cleanup, err := testutil.NewIsolatedDB("inferencegw")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testDB = client
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// tunnelCall is what the gateway asked the coordinator for.
type tunnelCall struct {
	RequestID string `json:"request_id"`
	ReplicaID string `json:"replica_id"`
	Stream    bool   `json:"stream"`
	Timeout   int    `json:"timeout_seconds"`
}

// harness is a gateway in front of a stand-in coordinator, with one
// deployment and as many serving replicas as a test asks for.
type harness struct {
	t        *testing.T
	g        *Gateway
	org, dep string
	key      string
	hosts    map[string]string // replica id → host id

	mu     sync.Mutex
	calls  []tunnelCall
	answer func(w http.ResponseWriter, c tunnelCall)
}

func newHarness(t *testing.T, replicas int) *harness {
	t.Helper()
	if testDB == nil {
		t.Skip("skipping: test database unavailable")
	}
	h := &harness{t: t, hosts: map[string]string{}}
	h.exec(`TRUNCATE request_failures, usage_events, replicas, gpus, deployments, hosts, api_keys CASCADE`)
	h.scan(&h.org, `INSERT INTO organizations (name) VALUES ('GatewayTestOrg') RETURNING id`)
	h.scan(&h.dep, `
		INSERT INTO deployments (org_id, model_id, name, tier, region, min_replicas, max_replicas, state)
		VALUES ($1, $2, 'chat', 't3', 'IN-SOUTH', 1, 4, 'serving') RETURNING id`, h.org, smallModel)
	for range replicas {
		var host, gpu, rep string
		h.scan(&host, `
			INSERT INTO hosts (name, tier, region, status, runtime, runtime_healthy, last_heartbeat_at)
			VALUES ('gw-' || gen_random_uuid(), 't3', 'IN-SOUTH', 'active', 'ollama', TRUE, NOW()) RETURNING id`)
		h.scan(&gpu, `INSERT INTO gpus (host_id, model, vram_gb, uuid) VALUES ($1, 'Test GPU', 8, 'GPU-' || gen_random_uuid()) RETURNING id`, host)
		h.scan(&rep, `INSERT INTO replicas (deployment_id, host_id, gpu_id, state) VALUES ($1, $2, $3, 'serving') RETURNING id`, h.dep, host, gpu)
		h.hosts[rep] = host
	}
	raw, prefix, hash, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	h.key = raw
	h.exec(`INSERT INTO api_keys (org_id, name, prefix, hash) VALUES ($1, 'test', $2, $3)`, h.org, prefix, hash)

	coordinator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var c tunnelCall
		_ = json.NewDecoder(r.Body).Decode(&c)
		h.mu.Lock()
		h.calls = append(h.calls, c)
		answer := h.answer
		h.mu.Unlock()
		answer(w, c)
	}))
	t.Cleanup(coordinator.Close)
	h.answer = func(w http.ResponseWriter, _ tunnelCall) { ok(w) }

	svc, err := auth.NewServiceAuthenticator("gateway-test-secret-0123456789abcdef", auth.ServiceInferenceGateway)
	if err != nil {
		t.Fatal(err)
	}
	h.g = &Gateway{
		db: testDB, router: newRouter(testDB, time.Minute), svcAuth: svc,
		coordinatorURL: coordinator.URL, client: coordinator.Client(),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return h
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := testDB.Pool.Exec(context.Background(), sql, args...); err != nil {
		h.t.Fatalf("%s: %v", sql, err)
	}
}

func (h *harness) scan(dest any, sql string, args ...any) {
	h.t.Helper()
	if err := testDB.Pool.QueryRow(context.Background(), sql, args...).Scan(dest); err != nil {
		h.t.Fatalf("%s: %v", sql, err)
	}
}

func (h *harness) count(sql string, args ...any) int {
	h.t.Helper()
	var n int
	h.scan(&n, sql, args...)
	return n
}

func (h *harness) tunnelled() []tunnelCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]tunnelCall(nil), h.calls...)
}

// ok answers as the coordinator does for a finished request.
func ok(w http.ResponseWriter) {
	w.Header().Set("Trailer", trailerPromptTokens+", "+trailerCompletionTokens+", "+trailerError)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	w.Header().Set(trailerPromptTokens, "4")
	w.Header().Set(trailerCompletionTokens, "2")
}

// refuseAs answers as the coordinator does when an attempt fails.
func refuseAs(w http.ResponseWriter, status int, reason string) {
	w.Header().Set(headerTunnelReason, reason)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"detail":"replica error: boom"}`))
}

func (h *harness) post(body string, headers ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.key)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.g.HandleInference(rec, req)
	return rec
}

const chat = `{"model":"chat","messages":[{"role":"user","content":"hello"}]}`

// A replica that fails is tried around, and the failure is written down
// against its host. Before this a host could fail every request it was given
// and keep a perfect record.
func TestAFailedAttemptIsRetriedElsewhereAndRecorded(t *testing.T) {
	h := newHarness(t, 2)
	var bad string
	h.answer = func(w http.ResponseWriter, c tunnelCall) {
		h.mu.Lock()
		if bad == "" {
			bad = c.ReplicaID
		}
		failing := c.ReplicaID == bad
		h.mu.Unlock()
		if failing {
			refuseAs(w, http.StatusBadGateway, "host_error")
			return
		}
		ok(w)
	}

	id := uuid.NewString()
	rec := h.post(chat, "X-Request-ID", id)
	if rec.Code != 200 {
		t.Fatalf("status %d body %s, want the answer from the other replica", rec.Code, rec.Body)
	}
	calls := h.tunnelled()
	if len(calls) != 2 || calls[0].ReplicaID == calls[1].ReplicaID {
		t.Fatalf("tunnel calls %+v, want one to each replica", calls)
	}
	// One id from the edge to the host, on every attempt.
	if rec.Header().Get("X-Request-ID") != id || calls[0].RequestID != id || calls[1].RequestID != id {
		t.Fatalf("request id changed on the way: header %q, attempts %q and %q, want %q",
			rec.Header().Get("X-Request-ID"), calls[0].RequestID, calls[1].RequestID, id)
	}
	if calls[0].Timeout != 600 {
		t.Fatalf("the coordinator was given a limit of %d s, want the one configured limit (600)", calls[0].Timeout)
	}

	var host, reason, kind string
	var status int
	if err := testDB.Pool.QueryRow(context.Background(),
		`SELECT host_id::TEXT, reason, kind, status_code FROM request_failures WHERE request_id = $1`, id).
		Scan(&host, &reason, &kind, &status); err != nil {
		t.Fatalf("the failed attempt was not recorded: %v", err)
	}
	if host != h.hosts[bad] || reason != "host_error" || kind != failedAttempt || status != 502 {
		t.Fatalf("recorded host %s reason %s kind %s status %d", host, reason, kind, status)
	}
	if n := h.count(`SELECT COUNT(*) FROM usage_events WHERE request_id = $1 AND status = 'success'`, id); n != 1 {
		t.Fatalf("%d usage rows for the answered request, want 1", n)
	}
	// The caller got an answer, so their error count is still zero.
	if n := h.count(`SELECT COUNT(*) FROM request_outcomes WHERE org_id = $1 AND status <> 'success'`, h.org); n != 0 {
		t.Fatalf("%d errors shown to a caller whose request was answered", n)
	}
}

func TestARequestNoReplicaCouldAnswerIsRecordedAsRefused(t *testing.T) {
	h := newHarness(t, 2)
	h.answer = func(w http.ResponseWriter, _ tunnelCall) { refuseAs(w, http.StatusGatewayTimeout, "host_timeout") }

	rec := h.post(chat)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "no_healthy_replica") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if n := h.count(`SELECT COUNT(*) FROM request_failures WHERE kind = 'attempt' AND reason = 'host_timeout'`); n != 2 {
		t.Fatalf("%d attempts recorded, want one per replica", n)
	}
	if n := h.count(`SELECT COUNT(*) FROM request_failures WHERE kind = 'refused' AND reason = 'no_healthy_replica'`); n != 1 {
		t.Fatalf("%d refusals recorded, want 1", n)
	}
	// The caller sees one failed request, not three and not none.
	if n := h.count(`SELECT COUNT(*) FROM request_outcomes WHERE deployment_id = $1 AND status = 'error' AND NOT served`, h.dep); n != 1 {
		t.Fatalf("the caller's error count is %d, want 1", n)
	}

	// Both replicas are now resting, and a deployment that is not ready is
	// written down too.
	h.exec(`UPDATE deployments SET state = 'pending' WHERE id = $1`, h.dep)
	if rec := h.post(chat); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d for a deployment that is not serving", rec.Code)
	}
	if n := h.count(`SELECT COUNT(*) FROM request_failures WHERE kind = 'refused' AND reason = 'deployment_not_ready'`); n != 1 {
		t.Fatalf("%d not-ready refusals recorded, want 1", n)
	}
}

// A replica answers a few requests at a time. Past its limit the caller is
// told to come back, which client libraries do by themselves on 429.
func TestAReplicaIsGivenNoMoreThanItsLimit(t *testing.T) {
	h := newHarness(t, 1)
	h.g.router.maxInflight = 1
	entered, leave := make(chan struct{}), make(chan struct{})
	h.answer = func(w http.ResponseWriter, _ tunnelCall) {
		entered <- struct{}{}
		<-leave
		ok(w)
	}

	first := make(chan int)
	go func() { first <- h.post(chat).Code }()
	<-entered

	rec := h.post(chat)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "at_capacity") {
		t.Fatalf("status %d retry-after %q body %s, want 429 at_capacity", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
	if n := len(h.tunnelled()); n != 1 {
		t.Fatalf("%d requests reached the replica, want 1", n)
	}
	close(leave)
	if code := <-first; code != 200 {
		t.Fatalf("the request that was in flight ended with %d", code)
	}
	// Being busy is not being broken: the replica takes the next request.
	go func() { <-entered }()
	if rec := h.post(chat); rec.Code != 200 {
		t.Fatalf("status %d once the replica was free again", rec.Code)
	}
	if n := h.count(`SELECT COUNT(*) FROM request_failures WHERE reason = 'at_capacity' AND host_id IS NULL`); n != 1 {
		t.Fatalf("%d capacity refusals recorded against no host, want 1", n)
	}
}

// If the coordinator itself disappears mid-stream, nothing upstream is left
// to say so. The gateway ends the stream with the error event.
func TestAStreamWhoseSourceDisappearsEndsWithAnErrorEvent(t *testing.T) {
	h := newHarness(t, 1)
	h.answer = func(w http.ResponseWriter, _ tunnelCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}

	rec := h.post(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: {\"choices\"") || !strings.Contains(body, `"code":"stream_interrupted"`) || strings.Contains(body, "[DONE]") {
		t.Fatalf("body %q, want the tokens that arrived and then an error event", body)
	}
	if n := h.count(`SELECT COUNT(*) FROM usage_events WHERE status = 'error'`); n != 1 {
		t.Fatalf("%d error rows for the broken stream, want 1", n)
	}
}

func TestBodiesUpToTheSharedLimitAreAccepted(t *testing.T) {
	h := newHarness(t, 1)
	big := func(n int) string {
		return `{"model":"chat","messages":[{"role":"user","content":"` + strings.Repeat("a", n) + `"}]}`
	}
	// 12 MB: over the old 8 MB limit, under the one every hop now shares.
	if rec := h.post(big(12 << 20)); rec.Code != 200 {
		t.Fatalf("status %d for a 12 MB body", rec.Code)
	}
	rec := h.post(big(17 << 20))
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "16 MB") {
		t.Fatalf("status %d body %s, want 413 naming the limit", rec.Code, rec.Body)
	}
	if n := len(h.tunnelled()); n != 1 {
		t.Fatalf("%d requests reached the coordinator, want only the one within the limit", n)
	}
}

func TestOnlyAnIDIssuedByThePlatformIsKept(t *testing.T) {
	h := newHarness(t, 1)
	rec := h.post(chat, "X-Request-ID", "not-a-uuid")
	got := rec.Header().Get("X-Request-ID")
	if _, err := uuid.Parse(got); err != nil {
		t.Fatalf("request id %q is not one the platform issued", got)
	}
}
