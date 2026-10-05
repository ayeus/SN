package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ayeus/ayeusann/internal/httpx"
	"github.com/jackc/pgx/v5"
)

// Who may create an account (SIGNUP_MODE).
const (
	SignupOpen   = "open"   // anyone who can reach the address
	SignupInvite = "invite" // only with an invitation link from an operator
	SignupClosed = "closed" // nobody
)

const (
	// inviteTTL is how long an invitation link works unless the operator says otherwise.
	inviteTTL = 72 * time.Hour
	// maxInviteTTL bounds what the operator may ask for.
	maxInviteTTL = 30 * 24 * time.Hour
	// operatorResetTTL is longer than an emailed reset link's hour: the operator
	// passes this one on by hand, and the person may not read it at once.
	operatorResetTTL = 24 * time.Hour
)

// Reasons a sign-up is turned away before an account exists.
var (
	errInviteRequired = errors.New("signup: invitation required")
	errSignupClosed   = errors.New("signup: closed")
	errOwnerCode      = errors.New("signup: wrong owner code")
	errOwnerClaimed   = errors.New("signup: installation already has an owner")
)

// inviteError is an invitation that cannot be used, with the reason to show.
type inviteError struct{ reason string }

func (e inviteError) Error() string { return "signup: " + e.reason }

func newToken(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// ownerCodeMatches compares in constant time. An installation with no owner
// code configured has no owner-code path at all.
func (a *API) ownerCodeMatches(code string) bool {
	code = strings.TrimSpace(code)
	return a.ownerCode != "" && code != "" && subtle.ConstantTimeCompare([]byte(code), []byte(a.ownerCode)) == 1
}

// ownerNeeded reports whether the installation is still waiting for its owner.
func (a *API) ownerNeeded(ctx context.Context) bool {
	if a.ownerCode == "" {
		return false
	}
	var claimed bool
	if err := a.db.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM installation);`).Scan(&claimed); err != nil {
		return false
	}
	return !claimed
}

// inviteProblem explains why an invitation link cannot be used, or returns ""
// when it can. email may be empty when the address is not known yet.
func inviteProblem(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, tokenHash, email string) string {
	var used, revoked, expired bool
	var forEmail *string
	err := q.QueryRow(ctx, `
		SELECT used_at IS NOT NULL, revoked_at IS NOT NULL, expires_at <= NOW(), email
		FROM invites WHERE token_hash = $1;
	`, tokenHash).Scan(&used, &revoked, &expired, &forEmail)
	switch {
	case err != nil:
		return "This invitation link is not valid. Ask for a new one."
	case used:
		return "This invitation link has already been used. Ask for a new one."
	case revoked:
		return "This invitation was withdrawn. Ask for a new one."
	case expired:
		return "This invitation link has expired. Ask for a new one."
	case forEmail != nil && email != "" && !strings.EqualFold(*forEmail, email):
		return "This invitation is for a different email address."
	}
	return ""
}

// HandleSignupInfo tells the sign-up page what it may offer: whether accounts
// are open, whether the installation still needs its owner, and whether the
// invitation in the link can be used. It reveals nothing about who is
// registered.
func (a *API) HandleSignupInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := map[string]any{
		"mode":         a.signupMode,
		"owner_needed": a.ownerNeeded(ctx),
	}
	if token := strings.TrimSpace(r.URL.Query().Get("invite")); token != "" {
		invite := map[string]any{"valid": false}
		hash := hashToken(token)
		if problem := inviteProblem(ctx, a.db.Pool, hash, ""); problem != "" {
			invite["problem"] = problem
		} else {
			var email, workspace *string
			var role string
			var expires time.Time
			if err := a.db.Pool.QueryRow(ctx, `
				SELECT i.email, o.name, i.role, i.expires_at
				FROM invites i LEFT JOIN organizations o ON o.id = i.org_id
				WHERE i.token_hash = $1;
			`, hash).Scan(&email, &workspace, &role, &expires); err == nil {
				invite = map[string]any{"valid": true, "email": email, "workspace": workspace, "role": role, "expires_at": expires}
			}
		}
		out["invite"] = invite
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── Invitations (operator) ───────────────────────────────────

type inviteView struct {
	ID        string     `json:"id"`
	State     string     `json:"state"` // pending, used, expired, revoked
	Workspace *string    `json:"workspace"`
	Role      string     `json:"role"`
	Email     *string    `json:"email"`
	Note      *string    `json:"note"`
	UsedBy    *string    `json:"used_by"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at"`
	CreatedAt time.Time  `json:"created_at"`
}

// HandleCreateInvite issues a single-use invitation link. The invited person
// either joins the operator's workspace, sharing its deployments and keys, or
// gets a workspace of their own.
func (a *API) HandleCreateInvite(w http.ResponseWriter, r *http.Request) {
	claims := claimsOf(r)
	var req struct {
		Workspace    string `json:"workspace"` // "new" (default) or "mine"
		Role         string `json:"role"`      // for "mine": member (default) or admin
		Email        string `json:"email"`
		Note         string `json:"note"`
		ExpiresHours int    `json:"expires_hours"`
	}
	_ = httpxDecodeOptional(r, &req)

	var orgID *string
	role := "admin" // of their own workspace
	switch strings.ToLower(strings.TrimSpace(req.Workspace)) {
	case "", "new":
	case "mine":
		orgID = &claims.OrgID
		role = strings.ToLower(strings.TrimSpace(req.Role))
		if role == "" {
			role = "member"
		}
		if role != "member" && role != "admin" {
			writeError(w, http.StatusBadRequest, "role must be member or admin")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "workspace must be new or mine")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email != "" && !validEmail(email) {
		httpx.WriteProblemFields(w, http.StatusBadRequest, "Enter a valid email address, or leave it empty", map[string]string{"email": "Enter a valid email address"})
		return
	}
	note := strings.TrimSpace(req.Note)
	if len(note) > 200 {
		note = note[:200]
	}
	ttl := inviteTTL
	if req.ExpiresHours > 0 {
		ttl = time.Duration(req.ExpiresHours) * time.Hour
	}
	if ttl > maxInviteTTL {
		ttl = maxInviteTTL
	}

	token, err := newToken(20)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not create the invitation")
		return
	}
	var id string
	var expires time.Time
	err = a.db.Pool.QueryRow(r.Context(), `
		INSERT INTO invites (token_hash, org_id, role, email, note, created_by, expires_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6, NOW() + ($7 * INTERVAL '1 second'))
		RETURNING id, expires_at;
	`, hashToken(token), orgID, role, email, note, claims.UserID, int(ttl.Seconds())).Scan(&id, &expires)
	if err != nil {
		a.log.Error("create invite failed", "err", err)
		writeError(w, http.StatusInternalServerError, "Could not create the invitation")
		return
	}
	a.audit(r, "invite.create", "invite", id, map[string]any{"workspace": req.Workspace, "email": email})
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id,
		// Shown once: only its hash is stored.
		"link":       a.publicBase(r) + "/signup?invite=" + token,
		"token":      token,
		"expires_at": expires,
	})
}

func (a *API) HandleListInvites(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Pool.Query(r.Context(), `
		SELECT i.id,
		       CASE WHEN i.used_at IS NOT NULL THEN 'used'
		            WHEN i.revoked_at IS NOT NULL THEN 'revoked'
		            WHEN i.expires_at <= NOW() THEN 'expired'
		            ELSE 'pending' END,
		       o.name, i.role, i.email, i.note, u.email, i.expires_at, i.used_at, i.created_at
		FROM invites i
		LEFT JOIN organizations o ON o.id = i.org_id
		LEFT JOIN users u ON u.id = i.used_by
		ORDER BY i.created_at DESC LIMIT 200;
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Database error")
		return
	}
	defer rows.Close()
	out := []inviteView{}
	for rows.Next() {
		var v inviteView
		if err := rows.Scan(&v.ID, &v.State, &v.Workspace, &v.Role, &v.Email, &v.Note, &v.UsedBy, &v.ExpiresAt, &v.UsedAt, &v.CreatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "Database error")
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": out})
}

// HandleRevokeInvite withdraws an invitation that has not been used.
func (a *API) HandleRevokeInvite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tag, err := a.db.Pool.Exec(r.Context(), `
		UPDATE invites SET revoked_at = NOW()
		WHERE id::TEXT = $1 AND used_at IS NULL AND revoked_at IS NULL;
	`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Database error")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "No unused invitation with that id")
		return
	}
	a.audit(r, "invite.revoke", "invite", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked", "id": id})
}

// ─── People (operator) ────────────────────────────────────────

type personView struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	Name       string     `json:"name"`
	Operator   bool       `json:"is_operator"`
	DisabledAt *time.Time `json:"disabled_at"`
	Workspaces string     `json:"workspaces"`
	Machines   int        `json:"machines"`
	CreatedAt  time.Time  `json:"created_at"`
}

func (a *API) HandleListPeople(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Pool.Query(r.Context(), `
		SELECT u.id, u.email, u.name, u.is_operator, u.disabled_at,
		       COALESCE((SELECT string_agg(o.name || ' (' || m.role || ')', ', ' ORDER BY m.created_at)
		                 FROM memberships m JOIN organizations o ON o.id = m.org_id
		                 WHERE m.user_id = u.id), ''),
		       (SELECT COUNT(*) FROM hosts h WHERE h.user_id = u.id AND h.deleted_at IS NULL),
		       u.created_at
		FROM users u WHERE u.deleted_at IS NULL
		ORDER BY u.created_at ASC LIMIT 500;
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Database error")
		return
	}
	defer rows.Close()
	out := []personView{}
	for rows.Next() {
		var p personView
		if err := rows.Scan(&p.ID, &p.Email, &p.Name, &p.Operator, &p.DisabledAt, &p.Workspaces, &p.Machines, &p.CreatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "Database error")
			return
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, map[string]any{"people": out})
}

// HandlePersonResetLink creates a password-reset link for the operator to pass
// on. A computer at home has no mail server, so "forgot password" cannot email
// anyone; this is the way back in.
func (a *API) HandlePersonResetLink(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	var email string
	if err := a.db.Pool.QueryRow(ctx, `SELECT email FROM users WHERE id::TEXT = $1 AND deleted_at IS NULL;`, id).Scan(&email); err != nil {
		writeError(w, http.StatusNotFound, "No such account")
		return
	}
	token, err := newToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not create a reset link")
		return
	}
	var expires time.Time
	if err := a.db.Pool.QueryRow(ctx, `
		INSERT INTO auth_tokens (user_id, kind, token_hash, expires_at)
		VALUES ($1::UUID, 'password_reset', $2, NOW() + ($3 * INTERVAL '1 second'))
		RETURNING expires_at;
	`, id, hashToken(token), int(operatorResetTTL.Seconds())).Scan(&expires); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not create a reset link")
		return
	}
	a.audit(r, "user.reset_link", "user", id, nil)
	writeJSON(w, http.StatusCreated, map[string]any{
		"email":      email,
		"link":       a.publicBase(r) + "/reset?token=" + token,
		"expires_at": expires,
	})
}

// HandleDisablePerson locks an account out: it cannot sign in, its sessions
// end now, and API keys of workspaces left with nobody who can sign in are
// revoked. Machines are not touched; ban those from the fleet list.
func (a *API) HandleDisablePerson(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	if id == claimsOf(r).UserID {
		writeError(w, http.StatusConflict, "You cannot disable your own account.")
		return
	}
	var userID string
	var keys int64
	err := a.db.ExecTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			UPDATE users SET disabled_at = COALESCE(disabled_at, NOW()), updated_at = NOW()
			WHERE id::TEXT = $1 AND deleted_at IS NULL RETURNING id;
		`, id).Scan(&userID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE api_keys k SET revoked = TRUE
			WHERE NOT k.revoked
			  AND k.org_id IN (SELECT org_id FROM memberships WHERE user_id = $1)
			  AND NOT EXISTS (
			      SELECT 1 FROM memberships m JOIN users u ON u.id = m.user_id
			      WHERE m.org_id = k.org_id AND u.deleted_at IS NULL AND u.disabled_at IS NULL);
		`, userID)
		keys = tag.RowsAffected()
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "No such account")
			return
		}
		writeError(w, http.StatusInternalServerError, "Could not disable the account")
		return
	}
	if err := a.revocations.RevokeAllForUser(ctx, userID, "account disabled by an operator"); err != nil {
		a.log.Error("failed to end sessions of a disabled account", "user", userID, "err", err)
	}
	a.audit(r, "user.disable", "user", userID, map[string]any{"api_keys_revoked": keys})
	writeJSON(w, http.StatusOK, map[string]any{"status": "disabled", "id": userID, "api_keys_revoked": keys})
}

// HandleEnablePerson lets a disabled account sign in again. Keys revoked when
// it was disabled stay revoked; new ones can be created.
func (a *API) HandleEnablePerson(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tag, err := a.db.Pool.Exec(r.Context(), `
		UPDATE users SET disabled_at = NULL, updated_at = NOW()
		WHERE id::TEXT = $1 AND deleted_at IS NULL AND disabled_at IS NOT NULL;
	`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not enable the account")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "No disabled account with that id")
		return
	}
	a.audit(r, "user.enable", "user", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "enabled", "id": id})
}
