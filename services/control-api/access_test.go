package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

const goodPassword = "Correct-Horse-9"

type problem struct {
	Detail string `json:"detail"`
}

func addr(prefix string) string { return prefix + "@" + uuid.NewString()[:8] + ".example.com" }

// signupWith attempts a sign-up with extra fields (invite, owner_code).
func (h *harness) signupWith(email string, extra map[string]string) (int, AuthResponse, string) {
	h.t.Helper()
	body := map[string]string{"email": email, "password": goodPassword, "name": "Test Person"}
	for k, v := range extra {
		body[k] = v
	}
	var raw json.RawMessage
	code := h.do("POST", "/v1/auth/signup", "", body, &raw)
	var resp AuthResponse
	var p problem
	_ = json.Unmarshal(raw, &resp)
	_ = json.Unmarshal(raw, &p)
	return code, resp, p.Detail
}

func (h *harness) login(email, password string) int {
	h.t.Helper()
	return h.do("POST", "/v1/auth/login", "", map[string]string{"email": email, "password": password}, nil)
}

// operator creates an account and marks it as an operator, the way the owner
// code does, then puts the installation into the given sign-up mode.
func (h *harness) operator(mode string) session {
	h.t.Helper()
	h.api.signupMode = SignupOpen
	s := h.signup("operator")
	if _, err := h.api.db.Pool.Exec(context.Background(), `UPDATE users SET is_operator = TRUE WHERE email = $1`, s.Email); err != nil {
		h.t.Fatal(err)
	}
	h.api.signupMode = mode
	return s
}

func (h *harness) invite(op session, body map[string]any) (id, token string) {
	h.t.Helper()
	var out struct {
		ID    string `json:"id"`
		Token string `json:"token"`
		Link  string `json:"link"`
	}
	if code := h.do("POST", "/v1/admin/invites", op.Token, body, &out); code != http.StatusCreated {
		h.t.Fatalf("create invite: %d", code)
	}
	if !strings.HasSuffix(out.Link, "/signup?invite="+out.Token) || !strings.HasPrefix(out.Link, "http://gateway.test/") {
		h.t.Fatalf("invite link = %q, want the public address and the token", out.Link)
	}
	return out.ID, out.Token
}

func (h *harness) sql(query string, args ...any) {
	h.t.Helper()
	if _, err := h.api.db.Pool.Exec(context.Background(), query, args...); err != nil {
		h.t.Fatal(err)
	}
}

func TestSignupIsByInvitation(t *testing.T) {
	h := newHarness(t)
	op := h.operator(SignupInvite)

	// No invitation, no account.
	stranger := addr("stranger")
	code, _, detail := h.signupWith(stranger, nil)
	if code != http.StatusForbidden || !strings.Contains(detail, "by invitation") {
		t.Fatalf("sign-up without an invitation = %d %q, want 403", code, detail)
	}
	if h.login(stranger, goodPassword) != http.StatusUnauthorized {
		t.Fatal("a refused sign-up still created an account")
	}
	if code, _, _ := h.signupWith(addr("guess"), map[string]string{"invite": "not-a-real-token"}); code != http.StatusForbidden {
		t.Fatalf("a made-up invitation was accepted: %d", code)
	}

	// An invitation works exactly once and gives its holder their own workspace.
	_, token := h.invite(op, nil)
	code, friend, detail := h.signupWith(addr("friend"), map[string]string{"invite": token})
	if code != http.StatusCreated {
		t.Fatalf("sign-up with an invitation = %d %q", code, detail)
	}
	if friend.Organization.ID == op.OrgID || friend.Role != "admin" {
		t.Fatalf("a plain invitation should create a workspace of the person's own: org %s role %s", friend.Organization.ID, friend.Role)
	}
	code, _, detail = h.signupWith(addr("second"), map[string]string{"invite": token})
	if code != http.StatusForbidden || !strings.Contains(detail, "already been used") {
		t.Fatalf("an invitation was used twice: %d %q", code, detail)
	}

	// The invited person is not an operator and cannot invite others.
	// (The operator's routes answer 404 to everyone else, so they cannot be probed.)
	if code := h.do("POST", "/v1/admin/invites", friend.AccessToken, nil, nil); code != http.StatusNotFound {
		t.Fatalf("a member created an invitation: %d", code)
	}
	if code := h.do("GET", "/v1/admin/people", friend.AccessToken, nil, nil); code != http.StatusNotFound {
		t.Fatalf("a member listed everyone's accounts: %d", code)
	}

	// Expired, withdrawn, and addressed to somebody else.
	id, token := h.invite(op, nil)
	h.sql(`UPDATE invites SET expires_at = NOW() - INTERVAL '1 minute' WHERE id = $1`, id)
	if code, _, detail := h.signupWith(addr("late"), map[string]string{"invite": token}); code != http.StatusForbidden || !strings.Contains(detail, "expired") {
		t.Fatalf("an expired invitation = %d %q", code, detail)
	}

	id, token = h.invite(op, nil)
	if code := h.do("DELETE", "/v1/admin/invites/"+id, op.Token, nil, nil); code != http.StatusOK {
		t.Fatalf("revoke: %d", code)
	}
	if code, _, detail := h.signupWith(addr("revoked"), map[string]string{"invite": token}); code != http.StatusForbidden || !strings.Contains(detail, "withdrawn") {
		t.Fatalf("a withdrawn invitation = %d %q", code, detail)
	}

	meant := addr("meant")
	_, token = h.invite(op, map[string]any{"email": strings.ToUpper(meant)})
	if code, _, detail := h.signupWith(addr("other"), map[string]string{"invite": token}); code != http.StatusForbidden || !strings.Contains(detail, "different email") {
		t.Fatalf("an invitation for someone else = %d %q", code, detail)
	}
	if code, _, detail := h.signupWith(meant, map[string]string{"invite": token}); code != http.StatusCreated {
		t.Fatalf("the person an invitation was for could not use it: %d %q", code, detail)
	}

	// A failed attempt (a weak password, a taken address) does not burn the link.
	_, token = h.invite(op, nil)
	var raw json.RawMessage
	if code := h.do("POST", "/v1/auth/signup", "", map[string]string{"email": addr("weak"), "password": "short", "name": "W", "invite": token}, &raw); code != http.StatusBadRequest {
		t.Fatalf("weak password: %d", code)
	}
	if code, _, _ := h.signupWith(op.Email, map[string]string{"invite": token}); code != http.StatusConflict {
		t.Fatalf("an address that is already registered: %d", code)
	}
	if code, _, detail := h.signupWith(addr("retry"), map[string]string{"invite": token}); code != http.StatusCreated {
		t.Fatalf("the invitation was used up by a failed attempt: %d %q", code, detail)
	}

	// The list shows what became of each one, and never the tokens.
	var list struct {
		Invites []map[string]any `json:"invites"`
	}
	h.do("GET", "/v1/admin/invites", op.Token, nil, &list)
	states := map[string]int{}
	for _, i := range list.Invites {
		states[i["state"].(string)]++
		if _, leaked := i["token"]; leaked {
			t.Fatal("the invitation list contains tokens")
		}
	}
	if states["used"] < 3 || states["expired"] < 1 || states["revoked"] < 1 {
		t.Fatalf("invitation states = %v", states)
	}
}

func TestClosedAndOpenSignup(t *testing.T) {
	h := newHarness(t)
	op := h.operator(SignupClosed)
	_, token := h.invite(op, nil)
	if code, _, detail := h.signupWith(addr("closed"), map[string]string{"invite": token}); code != http.StatusForbidden || !strings.Contains(detail, "not taking new accounts") {
		t.Fatalf("closed sign-up accepted an invitation: %d %q", code, detail)
	}
	if code, _, _ := h.signupWith(addr("closed"), nil); code != http.StatusForbidden {
		t.Fatalf("closed sign-up: %d", code)
	}

	h.api.signupMode = SignupOpen
	if code, _, _ := h.signupWith(addr("open"), nil); code != http.StatusCreated {
		t.Fatalf("open sign-up: %d", code)
	}
}

func TestInvitationIntoTheOperatorsWorkspace(t *testing.T) {
	h := newHarness(t)
	op := h.operator(SignupInvite)

	_, token := h.invite(op, map[string]any{"workspace": "mine"})
	code, mate, detail := h.signupWith(addr("mate"), map[string]string{"invite": token})
	if code != http.StatusCreated {
		t.Fatalf("join the operator's workspace: %d %q", code, detail)
	}
	if mate.Organization.ID != op.OrgID || mate.Role != "member" {
		t.Fatalf("joined org %s as %s, want %s as member", mate.Organization.ID, mate.Role, op.OrgID)
	}
	// Same workspace: what one creates the other sees.
	var key struct {
		ID string `json:"id"`
	}
	if code := h.do("POST", "/v1/api-keys", op.Token, map[string]string{"name": "shared"}, &key); code != http.StatusCreated {
		t.Fatalf("create key: %d", code)
	}
	var keys struct {
		Keys []struct {
			ID string `json:"id"`
		} `json:"api_keys"`
	}
	var raw json.RawMessage
	h.do("GET", "/v1/api-keys", mate.AccessToken, nil, &raw)
	_ = json.Unmarshal(raw, &keys)
	if !strings.Contains(string(raw), key.ID) {
		t.Fatalf("a workspace member cannot see the workspace's key: %s", raw)
	}
	// A member of the operator's workspace is still not an operator.
	if code := h.do("GET", "/v1/admin/people", mate.AccessToken, nil, nil); code != http.StatusNotFound {
		t.Fatalf("a workspace member reached the operator's pages: %d", code)
	}

	if code := h.do("POST", "/v1/admin/invites", op.Token, map[string]any{"workspace": "theirs"}, nil); code != http.StatusBadRequest {
		t.Fatalf("an unknown workspace choice: %d", code)
	}
}

func TestSignupInfo(t *testing.T) {
	h := newHarness(t)
	op := h.operator(SignupInvite)
	_, token := h.invite(op, map[string]any{"workspace": "mine", "email": "ada@example.com"})

	var info struct {
		Mode        string `json:"mode"`
		OwnerNeeded bool   `json:"owner_needed"`
		Invite      *struct {
			Valid     bool    `json:"valid"`
			Problem   string  `json:"problem"`
			Email     *string `json:"email"`
			Workspace *string `json:"workspace"`
			Role      string  `json:"role"`
		} `json:"invite"`
	}
	h.do("GET", "/v1/auth/signup-info", "", nil, &info)
	if info.Mode != SignupInvite || info.Invite != nil {
		t.Fatalf("signup-info without a link = %+v", info)
	}
	h.do("GET", "/v1/auth/signup-info?invite="+token, "", nil, &info)
	if info.Invite == nil || !info.Invite.Valid || info.Invite.Email == nil || *info.Invite.Email != "ada@example.com" ||
		info.Invite.Workspace == nil || info.Invite.Role != "member" {
		t.Fatalf("signup-info for a good link = %+v", info.Invite)
	}
	info.Invite = nil
	h.do("GET", "/v1/auth/signup-info?invite=nonsense", "", nil, &info)
	if info.Invite == nil || info.Invite.Valid || info.Invite.Problem == "" {
		t.Fatalf("signup-info for a bad link = %+v", info.Invite)
	}
}

// The first account that presents the owner code becomes the operator. Two
// people racing with the same code must end with one owner, not two.
func TestOwnerCodeMakesExactlyOneOperator(t *testing.T) {
	h := newHarness(t)
	h.api.signupMode = SignupInvite
	h.api.ownerCode = "owner-code-for-tests"
	h.sql(`DELETE FROM installation`)
	t.Cleanup(func() { h.sql(`DELETE FROM installation`) })

	var info struct {
		OwnerNeeded bool `json:"owner_needed"`
	}
	h.do("GET", "/v1/auth/signup-info", "", nil, &info)
	if !info.OwnerNeeded {
		t.Fatal("an unclaimed installation should say it needs its owner")
	}

	wrong := addr("wrong")
	if code, _, detail := h.signupWith(wrong, map[string]string{"owner_code": "not-the-code"}); code != http.StatusForbidden || !strings.Contains(detail, "owner code") {
		t.Fatalf("a wrong owner code = %d %q", code, detail)
	}
	if h.login(wrong, goodPassword) != http.StatusUnauthorized {
		t.Fatal("a wrong owner code still created an account")
	}

	const racers = 6
	emails := make([]string, racers)
	codes := make([]int, racers)
	tokens := make([]string, racers)
	var wg sync.WaitGroup
	for i := range racers {
		emails[i] = addr("owner")
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, resp, _ := h.signupWith(emails[i], map[string]string{"owner_code": "owner-code-for-tests"})
			codes[i], tokens[i] = code, resp.AccessToken
		}()
	}
	wg.Wait()

	winner := -1
	for i, code := range codes {
		switch code {
		case http.StatusCreated:
			if winner >= 0 {
				t.Fatalf("two accounts claimed the installation: %v", codes)
			}
			winner = i
		case http.StatusForbidden:
		default:
			t.Fatalf("racer %d got %d", i, code)
		}
	}
	if winner < 0 {
		t.Fatalf("nobody claimed the installation: %v", codes)
	}
	var operators int
	if err := h.api.db.Pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM users WHERE is_operator AND email = ANY($1)`, emails).Scan(&operators); err != nil || operators != 1 {
		t.Fatalf("%d operators among the racers (err %v), want 1", operators, err)
	}
	for i, email := range emails {
		want := http.StatusUnauthorized // the losers have no account at all
		if i == winner {
			want = http.StatusOK
		}
		if got := h.login(email, goodPassword); got != want {
			t.Fatalf("racer %d sign-in = %d, want %d", i, got, want)
		}
	}

	var me struct {
		Operator bool `json:"is_platform_admin"`
	}
	h.do("GET", "/v1/auth/me", tokens[winner], nil, &me)
	if !me.Operator {
		t.Fatal("the owner is not an operator")
	}
	// The code is spent.
	if code, _, detail := h.signupWith(addr("late"), map[string]string{"owner_code": "owner-code-for-tests"}); code != http.StatusForbidden || !strings.Contains(detail, "already has its owner") {
		t.Fatalf("the owner code worked twice: %d %q", code, detail)
	}
	h.do("GET", "/v1/auth/signup-info", "", nil, &info)
	if info.OwnerNeeded {
		t.Fatal("a claimed installation still asks for its owner")
	}
	// And the owner can invite.
	if code := h.do("POST", "/v1/admin/invites", tokens[winner], nil, nil); code != http.StatusCreated {
		t.Fatalf("the owner could not create an invitation: %d", code)
	}

	// With no code configured there is no owner path at all.
	h.api.ownerCode = ""
	h.sql(`DELETE FROM installation`)
	if code, _, _ := h.signupWith(addr("nocode"), map[string]string{"owner_code": ""}); code != http.StatusForbidden {
		t.Fatalf("an empty owner code was accepted: %d", code)
	}
}

// loginFrom signs in from a fixed address, which is what the limit is keyed on.
func (h *harness) loginFrom(remote, email, password string) int {
	h.t.Helper()
	b, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req := httptest.NewRequest("POST", "/v1/auth/login", bytes.NewReader(b))
	req.RemoteAddr = remote
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec.Code
}

// The limit has to stop a guesser, which means that once it is reached the
// right password is refused too.
func TestSignInLimit(t *testing.T) {
	h := newHarness(t)
	// Production allows ten attempts in ten minutes. Four keeps this test from
	// spending its time hashing passwords; the rule under test is the same.
	const limit = 4
	h.api.loginLimiter = newAttemptLimiter(limit, 10*time.Minute)
	s := h.signup("limited")
	const from = "203.0.113.7:5000"

	for i := range limit {
		if code := h.loginFrom(from, s.Email, "Wrong-Password-1"); code != http.StatusUnauthorized {
			t.Fatalf("wrong password #%d = %d, want 401", i+1, code)
		}
	}
	if code := h.loginFrom(from, s.Email, goodPassword); code != http.StatusTooManyRequests {
		t.Fatalf("the right password after the limit was reached = %d, want 429", code)
	}
	if code := h.loginFrom(from, s.Email, "Wrong-Password-1"); code != http.StatusTooManyRequests {
		t.Fatalf("one more wrong password = %d, want 429", code)
	}
	// Somebody else's guessing does not lock the owner out from their own address.
	if code := h.loginFrom("198.51.100.9:5000", s.Email, goodPassword); code != http.StatusOK {
		t.Fatalf("sign-in from another address = %d, want 200", code)
	}

	// Addresses that do not exist count as well.
	ghost := addr("ghost")
	for range limit {
		h.loginFrom(from, ghost, "Wrong-Password-1")
	}
	if code := h.loginFrom(from, ghost, "Wrong-Password-1"); code != http.StatusTooManyRequests {
		t.Fatalf("guessing an unregistered address was not limited: %d", code)
	}

	// A success clears the count, so occasional typos never add up to a lockout.
	other := h.signup("typos")
	const home = "203.0.113.8:5000"
	for round := range 3 {
		for range limit - 1 {
			h.loginFrom(home, other.Email, "Wrong-Password-1")
		}
		if code := h.loginFrom(home, other.Email, goodPassword); code != http.StatusOK {
			t.Fatalf("round %d: the right password just under the limit = %d, want 200", round+1, code)
		}
	}
}

func TestOperatorDisablesAndRestoresAnAccount(t *testing.T) {
	h := newHarness(t)
	op := h.operator(SignupInvite)
	_, token := h.invite(op, nil)
	email := addr("leaver")
	code, friend, _ := h.signupWith(email, map[string]string{"invite": token})
	if code != http.StatusCreated {
		t.Fatalf("sign-up: %d", code)
	}
	var key struct {
		ID string `json:"id"`
	}
	if code := h.do("POST", "/v1/api-keys", friend.AccessToken, map[string]string{"name": "mine"}, &key); code != http.StatusCreated {
		t.Fatalf("create key: %d", code)
	}

	var people struct {
		People []personView `json:"people"`
	}
	h.do("GET", "/v1/admin/people", op.Token, nil, &people)
	var id string
	for _, p := range people.People {
		if p.Email == email {
			id = p.ID
		}
	}
	if id == "" {
		t.Fatal("the new account is missing from the people list")
	}

	// Not by a member, and not yourself.
	if code := h.do("POST", "/v1/admin/people/"+id+"/disable", friend.AccessToken, nil, nil); code != http.StatusNotFound {
		t.Fatalf("a member disabled an account: %d", code)
	}
	var opID string
	for _, p := range people.People {
		if p.Email == op.Email {
			opID = p.ID
		}
	}
	if code := h.do("POST", "/v1/admin/people/"+opID+"/disable", op.Token, nil, nil); code != http.StatusConflict {
		t.Fatalf("the operator disabled their own account: %d", code)
	}

	var out struct {
		Keys int `json:"api_keys_revoked"`
	}
	if code := h.do("POST", "/v1/admin/people/"+id+"/disable", op.Token, nil, &out); code != http.StatusOK {
		t.Fatalf("disable: %d", code)
	}
	if out.Keys != 1 {
		t.Fatalf("%d API keys revoked, want the 1 in the workspace nobody can sign in to any more", out.Keys)
	}
	// Access tokens carry a whole-second timestamp; let the cutoff pass it.
	time.Sleep(1100 * time.Millisecond)
	if code := h.do("GET", "/v1/auth/me", friend.AccessToken, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("a disabled account's session still works: %d", code)
	}
	if code := h.do("POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": friend.RefreshToken}, nil); code != http.StatusUnauthorized {
		t.Fatalf("a disabled account refreshed its session: %d", code)
	}
	if code := h.login(email, goodPassword); code != http.StatusForbidden {
		t.Fatalf("a disabled account signed in: %d", code)
	}
	// Someone who does not know the password learns nothing about the account.
	if code := h.login(email, "Wrong-Password-1"); code != http.StatusUnauthorized {
		t.Fatalf("a wrong password on a disabled account = %d, want 401", code)
	}
	// The operator's own keys and session are untouched.
	if code := h.do("GET", "/v1/auth/me", op.Token, nil, nil); code != http.StatusOK {
		t.Fatalf("the operator's session: %d", code)
	}

	if code := h.do("POST", "/v1/admin/people/"+id+"/enable", op.Token, nil, nil); code != http.StatusOK {
		t.Fatalf("enable: %d", code)
	}
	if code := h.login(email, goodPassword); code != http.StatusOK {
		t.Fatalf("a restored account could not sign in: %d", code)
	}
	if code := h.do("POST", "/v1/admin/people/"+id+"/enable", op.Token, nil, nil); code != http.StatusNotFound {
		t.Fatalf("enabling an account that is not disabled: %d", code)
	}
}

// A computer at home has no mail server, so the operator hands out reset links.
func TestOperatorResetLink(t *testing.T) {
	h := newHarness(t)
	op := h.operator(SignupInvite)
	_, token := h.invite(op, nil)
	email := addr("forgetful")
	if code, _, _ := h.signupWith(email, map[string]string{"invite": token}); code != http.StatusCreated {
		t.Fatalf("sign-up: %d", code)
	}
	var id string
	if err := h.api.db.Pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email = $1`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}

	var out struct {
		Link  string `json:"link"`
		Email string `json:"email"`
	}
	if code := h.do("POST", "/v1/admin/people/"+id+"/reset-link", op.Token, nil, &out); code != http.StatusCreated {
		t.Fatalf("reset link: %d", code)
	}
	const marker = "/reset?token="
	if !strings.HasPrefix(out.Link, "http://gateway.test"+marker) || out.Email != email {
		t.Fatalf("reset link = %+v", out)
	}
	reset := out.Link[strings.Index(out.Link, marker)+len(marker):]
	const newPassword = "Brand-New-Horse-7"
	if code := h.do("POST", "/v1/auth/password/reset", "", map[string]string{"token": reset, "password": newPassword}, nil); code != http.StatusOK {
		t.Fatalf("using the link: %d", code)
	}
	if h.login(email, newPassword) != http.StatusOK || h.login(email, goodPassword) != http.StatusUnauthorized {
		t.Fatal("the password was not changed by the link")
	}
	if code := h.do("POST", "/v1/auth/password/reset", "", map[string]string{"token": reset, "password": "Another-Horse-55"}, nil); code != http.StatusBadRequest {
		t.Fatalf("a reset link worked twice: %d", code)
	}
	if code := h.do("POST", "/v1/admin/people/"+uuid.NewString()+"/reset-link", op.Token, nil, nil); code != http.StatusNotFound {
		t.Fatalf("a reset link for nobody: %d", code)
	}
}
