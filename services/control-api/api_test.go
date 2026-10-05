package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/testutil"
	"github.com/ayeus/ayeusann/internal/usage"
	"github.com/google/uuid"
)

const testJWTSecret = "test-secret-key-32-bytes-long-super-secure!"

type harness struct {
	t   *testing.T
	api *API
	h   http.Handler
}

func newHarness(t *testing.T) *harness {
	database := testutil.DB(t)
	tm, err := auth.NewTokenManager(testJWTSecret, 15*time.Minute, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	api := NewAPI(database, tm, auth.NewPGRevocationStore(database.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
		PublicURL:            "http://gateway.test",
		CoordinatorPublicURL: "http://coordinator.test:50051",
		HeartbeatTimeout:     15 * time.Second,
	})
	return &harness{t: t, api: api, h: api.Routes()}
}

// do sends a request and decodes the JSON response into out (if non-nil).
func (h *harness) do(method, path, token string, body any, out any, headers ...string) int {
	h.t.Helper()
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, buf)
	// Sign-in attempts are rate-limited per address; give every request its own.
	req.RemoteAddr = fmt.Sprintf("10.%d.%d.%d:4000", rand.IntN(250), rand.IntN(250), rand.IntN(250)+1)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			h.t.Fatalf("%s %s: invalid JSON (%d): %s", method, path, rec.Code, rec.Body.String())
		}
	}
	return rec.Code
}

type session struct {
	Token string
	OrgID string
	Email string
}

func (h *harness) signup(prefix string) session {
	h.t.Helper()
	var resp AuthResponse
	email := prefix + "@" + uuid.NewString()[:8] + ".example.com"
	code := h.do("POST", "/v1/auth/signup", "", map[string]string{
		"email": email, "password": "Correct-Horse-9", "name": "Test " + prefix,
	}, &resp)
	if code != http.StatusCreated {
		h.t.Fatalf("signup: status %d", code)
	}
	return session{Token: resp.AccessToken, OrgID: resp.Organization.ID, Email: email}
}

func TestSignupDefaultsToIndia(t *testing.T) {
	h := newHarness(t)
	var resp AuthResponse
	code := h.do("POST", "/v1/auth/signup", "", map[string]string{
		"email": "india@" + uuid.NewString()[:8] + ".example.com", "password": "Correct-Horse-9", "name": "Asha",
	}, &resp)
	if code != http.StatusCreated {
		t.Fatalf("status %d", code)
	}
	if resp.Organization.Country == nil || *resp.Organization.Country != "IN" || resp.Organization.DefaultRegion != "IN-SOUTH" {
		t.Fatalf("expected an Indian org in IN-SOUTH, got %v / %s", resp.Organization.Country, resp.Organization.DefaultRegion)
	}
	if resp.AccessToken == "" || resp.Role != "admin" {
		t.Fatalf("signup must return a session for the org admin, got role %q", resp.Role)
	}
}

func TestSignupValidationAndDuplicates(t *testing.T) {
	h := newHarness(t)
	var p struct {
		Fields map[string]string `json:"fields"`
	}
	if code := h.do("POST", "/v1/auth/signup", "", map[string]string{"email": "nope", "password": "short", "name": ""}, &p); code != http.StatusBadRequest {
		t.Fatalf("status %d", code)
	}
	for _, f := range []string{"email", "password", "name"} {
		if p.Fields[f] == "" {
			t.Errorf("missing field error for %s: %+v", f, p.Fields)
		}
	}

	email := "dupe@" + uuid.NewString()[:8] + ".example.com"
	body := map[string]string{"email": email, "password": "Correct-Horse-9", "name": "A"}
	if code := h.do("POST", "/v1/auth/signup", "", body, nil); code != http.StatusCreated {
		t.Fatalf("first signup %d", code)
	}
	if code := h.do("POST", "/v1/auth/signup", "", body, &p); code != http.StatusConflict {
		t.Fatalf("duplicate signup should be 409, got %d", code)
	}

	var login AuthResponse
	if code := h.do("POST", "/v1/auth/login", "", map[string]string{"email": strings.ToUpper(email), "password": "Correct-Horse-9"}, &login); code != http.StatusOK || login.AccessToken == "" {
		t.Fatalf("login failed: %d", code)
	}
	if code := h.do("POST", "/v1/auth/login", "", map[string]string{"email": email, "password": "wrong-password-1A"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("bad password should be 401, got %d", code)
	}
}

func TestDeploymentLifecycleAPI(t *testing.T) {
	h := newHarness(t)
	s := h.signup("dep")

	// Catalogue is public and seeded.
	var cat struct {
		Models []struct {
			ID    string   `json:"id"`
			Name  string   `json:"name"`
			Tiers []string `json:"tiers_allowed"`
		} `json:"models"`
	}
	if code := h.do("GET", "/v1/models", "", nil, &cat); code != http.StatusOK || len(cat.Models) == 0 {
		t.Fatalf("catalogue: %d, %d models (did you run the seeds?)", code, len(cat.Models))
	}
	var small, big string
	for _, m := range cat.Models {
		switch m.Name {
		case "qwen2.5-7b-instruct":
			small = m.ID
		case "llama-3.3-70b-instruct":
			big = m.ID
		}
	}

	// The 70B model is not allowed on T3 personal machines.
	var prob struct {
		Fields map[string]string `json:"fields"`
	}
	if code := h.do("POST", "/v1/deployments", s.Token, map[string]any{"model_id": big, "tier": "t3", "name": "too-big"}, &prob); code != http.StatusBadRequest || prob.Fields["tier"] == "" {
		t.Fatalf("70B on T3 should be rejected with a tier error, got %d %+v", code, prob)
	}

	// Create, with an Idempotency-Key replayed once.
	var created struct {
		Deployment DeploymentView `json:"deployment"`
		Secret     string         `json:"secret"`
		BaseURL    string         `json:"base_url"`
		Model      string         `json:"model"`
	}
	body := map[string]any{"model_id": small, "tier": "t3", "name": "chat-prod", "region": "IN-SOUTH"}
	if code := h.do("POST", "/v1/deployments", s.Token, body, &created, "Idempotency-Key", "k-1"); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if created.Deployment.State != "pending" || created.Deployment.DesiredState != "running" {
		t.Fatalf("new deployment should be pending/running, got %s/%s", created.Deployment.State, created.Deployment.DesiredState)
	}
	if !strings.HasPrefix(created.Secret, "sk_live_") || created.BaseURL != "http://gateway.test/v1" || created.Model != "chat-prod" {
		t.Fatalf("FR-22 endpoint and one-time key missing: %+v", created)
	}

	var replay struct {
		Deployment DeploymentView `json:"deployment"`
	}
	if code := h.do("POST", "/v1/deployments", s.Token, body, &replay, "Idempotency-Key", "k-1"); code != http.StatusCreated || replay.Deployment.ID != created.Deployment.ID {
		t.Fatalf("idempotent replay returned %d / %s, want the original %s", code, replay.Deployment.ID, created.Deployment.ID)
	}
	if code := h.do("POST", "/v1/deployments", s.Token, map[string]any{"model_id": small, "name": "other"}, nil, "Idempotency-Key", "k-1"); code != http.StatusUnprocessableEntity {
		t.Fatalf("key reuse with another body should be 422, got %d", code)
	}
	if code := h.do("POST", "/v1/deployments", s.Token, body, nil); code != http.StatusConflict {
		t.Fatalf("duplicate name should be 409, got %d", code)
	}

	// Detail, logs, and tenant isolation.
	path := "/v1/deployments/" + created.Deployment.ID
	if code := h.do("GET", path, s.Token, nil, nil); code != http.StatusOK {
		t.Fatalf("get: %d", code)
	}
	var logs struct {
		Events []struct{ Message string } `json:"events"`
	}
	h.do("GET", path+"/logs", s.Token, nil, &logs)
	if len(logs.Events) == 0 || !strings.Contains(logs.Events[0].Message, "Deployment created") {
		t.Fatalf("creation must be event-logged (FR-21): %+v", logs.Events)
	}
	other := h.signup("other")
	if code := h.do("GET", path, other.Token, nil, nil); code != http.StatusNotFound {
		t.Fatalf("another org must not see the deployment, got %d", code)
	}

	// Usage is counted per organisation: a served request shows up for its
	// owner and never for another tenant.
	var hostID, replicaID string
	ctx := t.Context()
	if err := h.api.db.Pool.QueryRow(ctx, `INSERT INTO hosts (name, tier, region) VALUES ('usage-' || gen_random_uuid(), 't3', 'IN-SOUTH') RETURNING id`).Scan(&hostID); err != nil {
		t.Fatal(err)
	}
	if err := h.api.db.Pool.QueryRow(ctx, `INSERT INTO replicas (deployment_id, host_id, state) VALUES ($1, $2, 'serving') RETURNING id`, created.Deployment.ID, hostID).Scan(&replicaID); err != nil {
		t.Fatal(err)
	}
	if _, err := usage.Record(ctx, h.api.db, usage.Event{
		RequestID: uuid.NewString(), OrgID: s.OrgID, DeploymentID: created.Deployment.ID, ReplicaID: replicaID, HostID: hostID,
		ModelID: created.Deployment.ModelID, Tier: "t3", InputTokens: 10, OutputTokens: 15,
	}); err != nil {
		t.Fatal(err)
	}
	var daily struct {
		Days []struct{ Requests, Tokens int64 } `json:"days"`
	}
	if code := h.do("GET", "/v1/usage/daily", s.Token, nil, &daily); code != http.StatusOK || len(daily.Days) != 1 || daily.Days[0].Requests != 1 || daily.Days[0].Tokens != 25 {
		t.Fatalf("daily usage: %d %+v, want one day with 1 request and 25 tokens", code, daily.Days)
	}
	if code := h.do("GET", "/v1/usage/daily", other.Token, nil, &daily); code != http.StatusOK || len(daily.Days) != 0 {
		t.Fatalf("another org sees this org's usage: %d %+v", code, daily.Days)
	}
	if _, err := h.api.db.Pool.Exec(ctx, `UPDATE replicas SET state = 'stopped' WHERE id = $1`, replicaID); err != nil {
		t.Fatal(err)
	}

	// Pause, bad resume/retry, stop.
	if code := h.do("PATCH", path, s.Token, map[string]string{"action": "retry"}, nil); code != http.StatusConflict {
		t.Fatalf("retry of a non-failed deployment should conflict, got %d", code)
	}
	var upd struct {
		Deployment DeploymentView `json:"deployment"`
	}
	if code := h.do("PATCH", path, s.Token, map[string]string{"action": "pause"}, &upd); code != http.StatusOK || upd.Deployment.DesiredState != "paused" {
		t.Fatalf("pause: %d %s", code, upd.Deployment.DesiredState)
	}
	if code := h.do("PATCH", path, s.Token, map[string]string{"action": "resume"}, &upd); code != http.StatusOK || upd.Deployment.DesiredState != "running" {
		t.Fatalf("resume: %d %s", code, upd.Deployment.DesiredState)
	}
	if code := h.do("DELETE", path, s.Token, nil, nil); code != http.StatusAccepted {
		t.Fatalf("stop: %d", code)
	}
}

func TestHostOnboardingAPI(t *testing.T) {
	h := newHarness(t)
	s := h.signup("host")

	var tok struct {
		Token    string            `json:"registration_token"`
		Tier     string            `json:"tier"`
		Commands map[string]string `json:"commands"`
	}
	if code := h.do("POST", "/v1/hosts/register-token", s.Token, map[string]string{"tier": "t3", "region": "IN-SOUTH"}, &tok); code != http.StatusCreated {
		t.Fatalf("register-token: %d", code)
	}
	claims, err := h.api.tm.VerifyRegistrationToken(tok.Token)
	if err != nil || claims.Tier != "t3" {
		t.Fatalf("token must be a registration token carrying the tier: %v %+v", err, claims)
	}
	if !strings.Contains(tok.Commands["linux_macos"], "http://gateway.test/install.sh") ||
		!strings.Contains(tok.Commands["linux_macos"], "--coordinator http://coordinator.test:50051") {
		t.Fatalf("install command wrong: %s", tok.Commands["linux_macos"])
	}

	// T1 is BD-led, never self-serve.
	if code := h.do("POST", "/v1/hosts/register-token", s.Token, map[string]string{"tier": "t1"}, nil); code != http.StatusForbidden {
		t.Fatalf("self-serve T1 should be forbidden, got %d", code)
	}

	// On a private network every machine is somebody's own computer: it joins
	// as Tier 3 unless the operator enrols it.
	if code := h.do("POST", "/v1/hosts/register-token", s.Token, map[string]string{"tier": "t2"}, nil); code != http.StatusCreated {
		t.Fatalf("self-serve T2 on a public installation: %d", code)
	}
	h.api.personalHostsOnly = true
	if code := h.do("POST", "/v1/hosts/register-token", s.Token, map[string]string{"tier": "t2"}, nil); code != http.StatusForbidden {
		t.Fatalf("self-serve T2 on a private network should be forbidden, got %d", code)
	}
	if code := h.do("POST", "/v1/hosts/register-token", s.Token, map[string]string{"tier": "t3"}, nil); code != http.StatusCreated {
		t.Fatalf("T3 on a private network: %d", code)
	}
	if code := h.do("POST", "/v1/hosts/register-token", s.Token, nil, &tok); code != http.StatusCreated || tok.Tier != "t3" {
		t.Fatalf("the default tier on a private network = %d %q, want t3", code, tok.Tier)
	}
	h.api.platformAdmins[strings.ToLower(s.Email)] = true
	if code := h.do("POST", "/v1/hosts/register-token", s.Token, map[string]string{"tier": "t2"}, nil); code != http.StatusCreated {
		t.Fatalf("the operator enrolling a T2 machine on a private network: %d", code)
	}
	delete(h.api.platformAdmins, strings.ToLower(s.Email))
	h.api.personalHostsOnly = false

	var hosts struct {
		Hosts []any `json:"hosts"`
	}
	if code := h.do("GET", "/v1/hosts", s.Token, nil, &hosts); code != http.StatusOK || len(hosts.Hosts) != 0 {
		t.Fatalf("new user should have no hosts: %d %d", code, len(hosts.Hosts))
	}
	if code := h.do("GET", "/v1/hosts/"+uuid.NewString(), s.Token, nil, nil); code != http.StatusNotFound {
		t.Fatalf("unknown host should 404, got %d", code)
	}

	// Public, aggregate-only endpoints.
	var stats map[string]any
	if code := h.do("GET", "/v1/network/stats", "", nil, &stats); code != http.StatusOK {
		t.Fatalf("network stats: %d", code)
	}
	for _, leaked := range []string{"hosts", "hw_fingerprint", "overlay_ip"} {
		if _, ok := stats[leaked]; ok {
			t.Fatalf("public stats must not expose %q", leaked)
		}
	}
	for _, key := range []string{"gpus_by_tier", "gpus_free_by_tier"} {
		if _, ok := stats[key].(map[string]any); !ok {
			t.Fatalf("network stats must report %s per tier, got %v", key, stats[key])
		}
	}
}

func TestBYORules(t *testing.T) {
	h := newHarness(t)
	s := h.signup("byo")
	base := map[string]any{"name": "my-ft-" + uuid.NewString()[:6], "family": "llama", "params_b": 8, "license": "llama3",
		"min_vram_gb": 16}

	withT3 := map[string]any{}
	for k, v := range base {
		withT3[k] = v
	}
	withT3["tiers_allowed"] = []string{"t2", "t3"}
	if code := h.do("POST", "/v1/models/byo", s.Token, withT3, nil); code != http.StatusForbidden {
		t.Fatalf("BYO on T3 must be forbidden (ADR-007), got %d", code)
	}
	if code := h.do("POST", "/v1/models/byo", s.Token, base, nil); code != http.StatusCreated {
		t.Fatalf("BYO register: %d", code)
	}
	if code := h.do("POST", "/v1/models/byo", s.Token, base, nil); code != http.StatusConflict {
		t.Fatalf("duplicate BYO name should be 409, got %d", code)
	}

	// Another tenant's catalogue does not include this BYO model.
	other := h.signup("byo-other")
	var cat struct {
		Models []struct{ Name string } `json:"models"`
	}
	h.do("GET", "/v1/models", other.Token, nil, &cat)
	for _, m := range cat.Models {
		if m.Name == base["name"] {
			t.Fatal("BYO model leaked to another organisation")
		}
	}
}

type captureMailer struct{ body string }

func (m *captureMailer) Send(_, _, body string) error { m.body = body; return nil }

func TestPasswordResetFlow(t *testing.T) {
	h := newHarness(t)
	mail := &captureMailer{}
	h.api.mailer = mail

	email := "reset@" + uuid.NewString()[:8] + ".example.com"
	if code := h.do("POST", "/v1/auth/signup", "", map[string]string{"email": email, "password": "Correct-Horse-9", "name": "R"}, nil); code != http.StatusCreated {
		t.Fatalf("signup %d", code)
	}
	var old AuthResponse
	h.do("POST", "/v1/auth/login", "", map[string]string{"email": email, "password": "Correct-Horse-9"}, &old)

	// An unknown address gets the same answer and no email.
	if code := h.do("POST", "/v1/auth/password/forgot", "", map[string]string{"email": "nobody@" + uuid.NewString()[:8] + ".example.com"}, nil); code != http.StatusOK || mail.body != "" {
		t.Fatalf("unknown email must look identical and send nothing: %d %q", code, mail.body)
	}
	if code := h.do("POST", "/v1/auth/password/forgot", "", map[string]string{"email": email}, nil); code != http.StatusOK {
		t.Fatalf("forgot %d", code)
	}
	i := strings.Index(mail.body, "/reset?token=")
	if i < 0 {
		t.Fatalf("no reset link in mail: %q", mail.body)
	}
	token := strings.Fields(mail.body[i+len("/reset?token="):])[0]

	if code := h.do("POST", "/v1/auth/password/reset", "", map[string]string{"token": token, "password": "weak"}, nil); code != http.StatusBadRequest {
		t.Fatalf("weak password should be rejected, got %d", code)
	}
	if code := h.do("POST", "/v1/auth/password/reset", "", map[string]string{"token": token, "password": "Brand-New-Pass-7"}, nil); code != http.StatusOK {
		t.Fatalf("reset %d", code)
	}
	if code := h.do("POST", "/v1/auth/password/reset", "", map[string]string{"token": token, "password": "Another-Pass-77"}, nil); code != http.StatusBadRequest {
		t.Fatalf("a reset link must work once, got %d", code)
	}
	if code := h.do("POST", "/v1/auth/login", "", map[string]string{"email": email, "password": "Correct-Horse-9"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("old password still works: %d", code)
	}
	if code := h.do("POST", "/v1/auth/login", "", map[string]string{"email": email, "password": "Brand-New-Pass-7"}, nil); code != http.StatusOK {
		t.Fatalf("new password rejected: %d", code)
	}
	// Sessions issued before the reset are dead.
	time.Sleep(1100 * time.Millisecond)
	if code := h.do("GET", "/v1/auth/me", old.AccessToken, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("pre-reset session should be invalid, got %d", code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	l := newAttemptLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !l.allow("k") {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
	if l.allow("k") {
		t.Fatal("4th attempt within the window must be blocked")
	}
	if !l.allow("other") {
		t.Fatal("limits are per key")
	}
}

func TestInstallTokenStatus(t *testing.T) {
	h := newHarness(t)
	s := h.signup("tok")
	var tok struct {
		ID string `json:"token_id"`
	}
	if code := h.do("POST", "/v1/hosts/register-token", s.Token, map[string]string{"tier": "t3"}, &tok); code != http.StatusCreated || tok.ID == "" {
		t.Fatalf("register-token: %d %+v", code, tok)
	}
	var st struct {
		Used   bool    `json:"used"`
		HostID *string `json:"host_id"`
	}
	if code := h.do("GET", "/v1/host-tokens/"+tok.ID, s.Token, nil, &st); code != http.StatusOK || st.Used || st.HostID != nil {
		t.Fatalf("fresh token should be unused: %d %+v", code, st)
	}
	other := h.signup("tok-other")
	if code := h.do("GET", "/v1/host-tokens/"+tok.ID, other.Token, nil, nil); code != http.StatusNotFound {
		t.Fatalf("another user must not see the token, got %d", code)
	}
}
