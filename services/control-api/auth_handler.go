package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/domain"
	"github.com/ayeus/ayeusann/internal/httpx"
	"github.com/ayeus/ayeusann/internal/platform"
	"github.com/jackc/pgx/v5"
)

type SignupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
	OrgName  string `json:"org_name,omitempty"`
	// Country is ISO 3166-1 alpha-2. It picks the default region. India is the
	// launch market (PRD §1).
	Country string `json:"country,omitempty"`
	Region  string `json:"region,omitempty"`
}

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type AuthResponse struct {
	AccessToken  string              `json:"access_token"`
	RefreshToken string              `json:"refresh_token"`
	TokenType    string              `json:"token_type"`
	ExpiresIn    int                 `json:"expires_in"`
	User         domain.User         `json:"user"`
	Organization domain.Organization `json:"organization"`
	Role         string              `json:"role"`
}

const orgColumns = `o.id, o.name, o.default_region, o.billing_country, o.created_at, o.updated_at`

func scanOrg(row pgx.Row, extra ...any) (domain.Organization, error) {
	var o domain.Organization
	dest := append([]any{&o.ID, &o.Name, &o.DefaultRegion, &o.Country, &o.CreatedAt, &o.UpdatedAt}, extra...)
	err := row.Scan(dest...)
	return o, err
}

func (a *API) HandleSignup(w http.ResponseWriter, r *http.Request) {
	var req SignupRequest
	if httpx.DecodeJSON(w, r, &req) != nil {
		return
	}

	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.Name = strings.TrimSpace(req.Name)
	fields := map[string]string{}
	if !validEmail(req.Email) {
		fields["email"] = "Enter a valid email address"
	}
	if req.Name == "" {
		fields["name"] = "Name is required"
	}
	if err := auth.ValidatePassword(req.Password); err != nil {
		fields["password"] = strings.TrimPrefix(err.Error(), "auth: ")
	}
	country := normalizeCountry(req.Country)
	region := req.Region
	if region == "" {
		region = defaultRegionForCountry(country)
	}
	if !domain.IsValidRegion(region) {
		fields["region"] = "Unknown region"
	}
	if len(fields) > 0 {
		httpx.WriteProblemFields(w, http.StatusBadRequest, "Please correct the highlighted fields", fields)
		return
	}

	passwordHash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.OrgName == "" {
		req.OrgName = req.Name + "'s workspace"
	}

	ctx := r.Context()
	var user domain.User
	var org domain.Organization

	err = a.db.ExecTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO users (email, password_hash, name, auth_provider)
			VALUES ($1, $2, $3, 'email')
			RETURNING id, email, name, auth_provider, email_verified, created_at, updated_at;
		`, req.Email, passwordHash, req.Name).Scan(
			&user.ID, &user.Email, &user.Name, &user.AuthProvider, &user.EmailVerified, &user.CreatedAt, &user.UpdatedAt,
		)
		if err != nil {
			if isUniqueViolation(err) {
				return errEmailTaken
			}
			return fmt.Errorf("create user: %w", err)
		}

		org, err = scanOrg(tx.QueryRow(ctx, `
			INSERT INTO organizations (name, default_region, billing_country)
			VALUES ($1, $2, $3)
			RETURNING id, name, default_region, billing_country, created_at, updated_at;
		`, req.OrgName, region, country))
		if err != nil {
			return fmt.Errorf("create organization: %w", err)
		}

		if _, err := tx.Exec(ctx, `INSERT INTO memberships (user_id, org_id, role) VALUES ($1, $2, 'admin');`, user.ID, org.ID); err != nil {
			return fmt.Errorf("create membership: %w", err)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errEmailTaken) {
			httpx.WriteProblemFields(w, http.StatusConflict, "An account with this email already exists",
				map[string]string{"email": "Already registered — sign in instead"})
			return
		}
		a.log.Error("signup failed", "err", err)
		writeError(w, http.StatusInternalServerError, "Could not create the account")
		return
	}

	a.issueSession(w, http.StatusCreated, user, org, domain.RoleAdmin)
}

var errEmailTaken = errors.New("email already registered")

func (a *API) issueSession(w http.ResponseWriter, status int, user domain.User, org domain.Organization, role string) {
	pair, err := a.tm.GenerateTokenPair(user.ID, org.ID, role)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to generate authentication tokens")
		return
	}
	writeJSON(w, status, AuthResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    int(a.tm.AccessExpiry().Seconds()),
		User:         user,
		Organization: org,
		Role:         role,
	})
}

var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s.]+(\.[^@\s.]+)+$`)

func validEmail(s string) bool { return len(s) <= 254 && emailPattern.MatchString(s) }

// clientIP returns the caller's address for rate limiting. X-Forwarded-For
// is trusted only behind a known proxy, since a client can forge it.
func clientIP(r *http.Request) *string {
	if platform.EnvBool("TRUST_PROXY_HEADERS", false) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if net.ParseIP(first) != nil {
				return &first
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if net.ParseIP(host) == nil {
		return nil
	}
	return &host
}

// normalizeCountry defaults to India, the launch market (PRD §1 decision 1).
func normalizeCountry(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	if len(c) != 2 {
		return "IN"
	}
	return c
}

// defaultRegionForCountry picks the nearest region; customers can override it
// per deployment.
func defaultRegionForCountry(country string) string {
	switch country {
	case "IN":
		return domain.RegionInSouth
	case "GB", "IE":
		return domain.RegionUKSouth
	case "DE", "FR", "NL", "ES", "IT", "PL", "SE", "NO", "DK", "FI", "BE", "AT", "CH":
		return domain.RegionEUCentral
	case "SG", "MY", "TH", "ID", "PH", "VN":
		return domain.RegionAPSouth
	case "JP", "KR", "TW":
		return domain.RegionAPNortheast
	case "AU", "NZ":
		return domain.RegionAPSoutheast
	case "BR", "AR", "CL", "CO":
		return domain.RegionSAEast
	case "CA":
		return domain.RegionCACentral
	case "AE", "SA", "QA", "KW":
		return domain.RegionMECentral
	case "ZA", "NG", "KE", "EG":
		return domain.RegionAFSouth
	default:
		return domain.RegionUSEast
	}
}

func (a *API) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if httpx.DecodeJSON(w, r, &req) != nil {
		return
	}
	ctx := r.Context()
	limitKey := strings.ToLower(strings.TrimSpace(req.Email))
	if ip := clientIP(r); ip != nil {
		limitKey = *ip + "|" + limitKey
	}

	var user domain.User
	var passwordHash *string
	err := a.db.Pool.QueryRow(ctx, `
		SELECT id, email, password_hash, name, auth_provider, email_verified, created_at, updated_at
		FROM users WHERE email = $1 AND deleted_at IS NULL;
	`, strings.ToLower(strings.TrimSpace(req.Email))).Scan(
		&user.ID, &user.Email, &passwordHash, &user.Name, &user.AuthProvider, &user.EmailVerified, &user.CreatedAt, &user.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Hash anyway so response time does not reveal which emails exist.
			auth.DummyPasswordCheck()
			writeError(w, http.StatusUnauthorized, "Invalid email or password")
			return
		}
		writeError(w, http.StatusInternalServerError, "Database error")
		return
	}
	if passwordHash == nil || !auth.CheckPasswordHash(req.Password, *passwordHash) {
		// Only failures count toward the limit, so a correct password always
		// works until the limit is hit by guesses.
		if !a.loginLimiter.allow(limitKey) {
			writeError(w, http.StatusTooManyRequests, "Too many failed sign-in attempts. Wait 10 minutes or reset your password.")
			return
		}
		platform.AuthFailuresTotal.WithLabelValues("control-api", "bad_password").Inc()
		writeError(w, http.StatusUnauthorized, "Invalid email or password")
		return
	}

	var role string
	org, err := scanOrg(a.db.Pool.QueryRow(ctx, `
		SELECT `+orgColumns+`, m.role
		FROM organizations o JOIN memberships m ON m.org_id = o.id
		WHERE m.user_id = $1 AND o.deleted_at IS NULL
		ORDER BY m.created_at ASC LIMIT 1;
	`, user.ID), &role)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to resolve your organisation")
		return
	}
	a.issueSession(w, http.StatusOK, user, org, role)
}

type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// HandleRefresh rotates a refresh token. A replayed refresh token is treated as
// theft and invalidates every session of the user.
func (a *API) HandleRefresh(w http.ResponseWriter, r *http.Request) {
	var req RefreshRequest
	if httpx.DecodeJSON(w, r, &req) != nil {
		return
	}
	claims, err := a.tm.VerifyRefreshToken(req.RefreshToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "Invalid or expired refresh token")
		return
	}
	ctx := r.Context()

	expiresAt := time.Now().Add(a.tm.RefreshExpiry())
	if claims.ExpiresAt != nil {
		expiresAt = claims.ExpiresAt.Time
	}
	if err := a.revocations.Consume(ctx, claims.ID, expiresAt); err != nil {
		if errors.Is(err, auth.ErrTokenAlreadyUsed) {
			_ = a.revocations.RevokeAllForUser(ctx, claims.UserID, "refresh token replay detected")
			writeError(w, http.StatusUnauthorized, "Refresh token has already been used; please sign in again")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "Unable to verify credential state")
		return
	}
	if cutoff, cErr := a.revocations.IssuedBefore(ctx, claims.UserID); cErr == nil &&
		!cutoff.IsZero() && claims.IssuedAt != nil && claims.IssuedAt.Before(cutoff) {
		writeError(w, http.StatusUnauthorized, "Credentials were invalidated; please sign in again")
		return
	}

	// Re-read the role so a demotion takes effect on the next refresh.
	var role string
	if err := a.db.Pool.QueryRow(ctx, `
		SELECT m.role FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.user_id = $1 AND m.org_id = $2 AND u.deleted_at IS NULL;
	`, claims.UserID, claims.OrgID).Scan(&role); err != nil {
		writeError(w, http.StatusUnauthorized, "Membership is no longer valid")
		return
	}

	pair, err := a.tm.GenerateTokenPair(claims.UserID, claims.OrgID, role)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to generate authentication tokens")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  pair.AccessToken,
		"refresh_token": pair.RefreshToken,
		"token_type":    "Bearer",
		"expires_in":    int(a.tm.AccessExpiry().Seconds()),
	})
}

// HandleLogout revokes the presented access token and, if supplied, the
// refresh token, so the client cannot mint a new session.
func (a *API) HandleLogout(w http.ResponseWriter, r *http.Request) {
	claims := claimsOf(r)
	ctx := r.Context()
	exp := time.Now().Add(a.tm.AccessExpiry())
	if claims.ExpiresAt != nil {
		exp = claims.ExpiresAt.Time
	}
	if err := a.revocations.Revoke(ctx, claims.ID, exp, "logout"); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to revoke session")
		return
	}
	var req RefreshRequest
	if err := httpxDecodeOptional(r, &req); err == nil && req.RefreshToken != "" {
		if rc, rErr := a.tm.VerifyRefreshToken(req.RefreshToken); rErr == nil && rc.UserID == claims.UserID {
			rexp := time.Now().Add(a.tm.RefreshExpiry())
			if rc.ExpiresAt != nil {
				rexp = rc.ExpiresAt.Time
			}
			_ = a.revocations.Revoke(ctx, rc.ID, rexp, "logout")
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

// HandleLogoutAll invalidates every token issued to the caller.
func (a *API) HandleLogoutAll(w http.ResponseWriter, r *http.Request) {
	if err := a.revocations.RevokeAllForUser(r.Context(), claimsOf(r).UserID, "user requested sign-out everywhere"); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to invalidate sessions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "all_sessions_invalidated"})
}

// HandleMe returns the signed-in user, their organisation and role.
func (a *API) HandleMe(w http.ResponseWriter, r *http.Request) {
	claims := claimsOf(r)
	ctx := r.Context()
	var user domain.User
	if err := a.db.Pool.QueryRow(ctx, `
		SELECT id, email, name, auth_provider, email_verified, created_at, updated_at
		FROM users WHERE id = $1 AND deleted_at IS NULL;
	`, claims.UserID).Scan(&user.ID, &user.Email, &user.Name, &user.AuthProvider, &user.EmailVerified, &user.CreatedAt, &user.UpdatedAt); err != nil {
		writeError(w, http.StatusNotFound, "User not found")
		return
	}
	org, err := scanOrg(a.db.Pool.QueryRow(ctx, `SELECT `+orgColumns+` FROM organizations o WHERE o.id = $1;`, claims.OrgID))
	if err != nil {
		writeError(w, http.StatusNotFound, "Organisation not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":              user,
		"organization":      org,
		"role":              claims.Role,
		"is_platform_admin": a.isPlatformAdmin(ctx, claims.UserID),
	})
}

// ─── Organisation ─────────────────────────────────────────────

func (a *API) HandleGetOrg(w http.ResponseWriter, r *http.Request) {
	org, err := scanOrg(a.db.Pool.QueryRow(r.Context(), `SELECT `+orgColumns+` FROM organizations o WHERE o.id = $1 AND o.deleted_at IS NULL;`, r.PathValue("id")))
	if err != nil {
		writeError(w, http.StatusNotFound, "Organisation not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"organization": org})
}

type UpdateOrgRequest struct {
	Name          *string `json:"name"`
	DefaultRegion *string `json:"default_region"`
}

func (a *API) HandleUpdateOrg(w http.ResponseWriter, r *http.Request) {
	var req UpdateOrgRequest
	if httpx.DecodeJSON(w, r, &req) != nil {
		return
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		writeError(w, http.StatusBadRequest, "Name must not be empty")
		return
	}
	if req.DefaultRegion != nil && !domain.IsValidRegion(*req.DefaultRegion) {
		writeError(w, http.StatusBadRequest, "Unknown region")
		return
	}
	org, err := scanOrg(a.db.Pool.QueryRow(r.Context(), `
		UPDATE organizations o
		SET name = COALESCE($2, o.name), default_region = COALESCE($3, o.default_region), updated_at = NOW()
		WHERE o.id = $1
		RETURNING `+orgColumns+`;
	`, r.PathValue("id"), req.Name, req.DefaultRegion))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update organisation")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"organization": org})
}

// ─── API keys ─────────────────────────────────────────────────

type CreateAPIKeyRequest struct {
	Name string `json:"name"`
	// DeploymentID scopes the key to one deployment (SRS FR-5).
	DeploymentID *string `json:"deployment_id,omitempty"`
}

func (a *API) HandleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	claims := claimsOf(r)
	var req CreateAPIKeyRequest
	if httpx.DecodeJSON(w, r, &req) != nil {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 100 {
		httpx.WriteProblemFields(w, http.StatusBadRequest, "A key name is required", map[string]string{"name": "1–100 characters"})
		return
	}
	key, secret, err := a.createAPIKey(r.Context(), a.db.Pool, claims.OrgID, req.Name, req.DeploymentID)
	if err != nil {
		if errors.Is(err, errDeploymentNotInOrg) {
			writeError(w, http.StatusNotFound, "Deployment not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to create API key")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"api_key": key, "secret": secret})
}

var errDeploymentNotInOrg = errors.New("deployment not in organisation")

// createAPIKey issues a key; the raw secret is returned exactly once.
func (a *API) createAPIKey(ctx context.Context, q db.Querier, orgID, name string, deploymentID *string) (domain.ApiKey, string, error) {
	scope := "org"
	if deploymentID != nil && *deploymentID != "" {
		var ok bool
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployments WHERE id = $1 AND org_id = $2);`,
			*deploymentID, orgID).Scan(&ok); err != nil || !ok {
			return domain.ApiKey{}, "", errDeploymentNotInOrg
		}
		scope = "deployment"
	} else {
		deploymentID = nil
	}

	raw, prefix, hash, err := auth.GenerateAPIKey()
	if err != nil {
		return domain.ApiKey{}, "", err
	}
	var k domain.ApiKey
	err = q.QueryRow(ctx, `
		INSERT INTO api_keys (org_id, name, prefix, hash, scope, deployment_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, org_id, name, prefix, scope, deployment_id, permissions, revoked, created_at;
	`, orgID, name, prefix, hash, scope, deploymentID).Scan(
		&k.ID, &k.OrgID, &k.Name, &k.Prefix, &k.Scope, &k.DeploymentID, &k.Permissions, &k.Revoked, &k.CreatedAt,
	)
	return k, raw, err
}

func (a *API) HandleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Pool.Query(r.Context(), `
		SELECT id, org_id, name, prefix, scope, deployment_id, permissions, last_used_at, revoked, created_at
		FROM api_keys WHERE org_id = $1 AND NOT revoked ORDER BY created_at DESC;
	`, claimsOf(r).OrgID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list API keys")
		return
	}
	defer rows.Close()
	keys := []domain.ApiKey{}
	for rows.Next() {
		var k domain.ApiKey
		if err := rows.Scan(&k.ID, &k.OrgID, &k.Name, &k.Prefix, &k.Scope, &k.DeploymentID, &k.Permissions,
			&k.LastUsedAt, &k.Revoked, &k.CreatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to read API keys")
			return
		}
		keys = append(keys, k)
	}
	writeJSON(w, http.StatusOK, map[string]any{"api_keys": keys, "count": len(keys)})
}

func (a *API) HandleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	tag, err := a.db.Pool.Exec(r.Context(),
		`UPDATE api_keys SET revoked = TRUE WHERE id = $1 AND org_id = $2 AND NOT revoked;`,
		r.PathValue("id"), claimsOf(r).OrgID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to revoke API key")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "API key not found or already revoked")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked", "id": r.PathValue("id")})
}
