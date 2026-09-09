package main

import (
	"context"
	"encoding/json"
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
	"github.com/ayeus/ayeusann/internal/money"
	"github.com/ayeus/ayeusann/internal/platform"
	"github.com/jackc/pgx/v5"
)

type AuthHandler struct {
	db          *db.Client
	tm          *auth.TokenManager
	revocations auth.RevocationStore
	ledger      *db.LedgerService
}

func NewAuthHandler(database *db.Client, tm *auth.TokenManager, revocations auth.RevocationStore, ledger *db.LedgerService) *AuthHandler {
	return &AuthHandler{
		db:          database,
		tm:          tm,
		revocations: revocations,
		ledger:      ledger,
	}
}

type SignupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
	OrgName  string `json:"org_name,omitempty"`
	// Country is an ISO 3166-1 alpha-2 code used to pick a default region,
	// billing currency, and tax jurisdiction. Optional; defaults to US.
	Country string `json:"country,omitempty"`
	// Region overrides the region derived from Country.
	Region string `json:"region,omitempty"`
}

// signupGrant is the promotional credit issued on signup. It is granted at most
// once per user and is rate-limited per signup IP and per email domain, because
// an unconditional $50 per account is a standing invitation to farm free compute.
var signupGrant = money.MustParse("50", "USD")

const (
	maxGrantsPerIPPerDay     = 3
	maxGrantsPerDomainPerDay = 25
)

type AuthResponse struct {
	AccessToken  string              `json:"access_token"`
	RefreshToken string              `json:"refresh_token"`
	TokenType    string              `json:"token_type"`
	ExpiresIn    int                 `json:"expires_in"`
	User         domain.User         `json:"user"`
	Organization domain.Organization `json:"organization"`
	// PromotionalCredit is set only when a signup grant was actually issued.
	PromotionalCredit string `json:"promotional_credit,omitempty"`
}

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *AuthHandler) HandleSignup(w http.ResponseWriter, r *http.Request) {
	var req SignupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if req.Email == "" || req.Password == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, "Email, password, and name are required")
		return
	}
	if !validEmail(req.Email) {
		writeError(w, http.StatusBadRequest, "A valid email address is required")
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))

	passwordHash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.OrgName == "" {
		req.OrgName = req.Name + "'s Org"
	}

	country := normalizeCountry(req.Country)
	region := req.Region
	if region == "" {
		region = defaultRegionForCountry(country)
	}
	currency := currencyForCountry(country)
	emailDomain := emailDomainOf(req.Email)
	signupIP := clientIP(r)

	ctx := r.Context()
	var user domain.User
	var org domain.Organization
	grantIssued := false

	// Transactionally create User + Organization + Membership (Admin) + signup credit.
	err = h.db.ExecTx(ctx, func(tx pgx.Tx) error {
		// 1. Create User
		userQuery := `
			INSERT INTO users (email, password_hash, name, auth_provider)
			VALUES ($1, $2, $3, 'email')
			RETURNING id, email, name, auth_provider, email_verified, created_at, updated_at;
		`
		err := tx.QueryRow(ctx, userQuery, req.Email, passwordHash, req.Name).Scan(
			&user.ID, &user.Email, &user.Name, &user.AuthProvider, &user.EmailVerified, &user.CreatedAt, &user.UpdatedAt,
		)
		if err != nil {
			return fmt.Errorf("failed to create user (email may already be registered): %w", err)
		}

		// 2. Create Organization
		orgQuery := `
			INSERT INTO organizations (name, default_region, billing_country, currency)
			VALUES ($1, $2, $3, $4)
			RETURNING id, name, default_region, created_at, updated_at;
		`
		err = tx.QueryRow(ctx, orgQuery, req.OrgName, region, country, currency).Scan(
			&org.ID, &org.Name, &org.DefaultRegion, &org.CreatedAt, &org.UpdatedAt,
		)
		if err != nil {
			return fmt.Errorf("failed to create organization: %w", err)
		}

		// 3. Create Membership (Role: Admin)
		memQuery := `
			INSERT INTO memberships (user_id, org_id, role)
			VALUES ($1, $2, $3);
		`
		_, err = tx.Exec(ctx, memQuery, user.ID, org.ID, domain.RoleAdmin)
		if err != nil {
			return fmt.Errorf("failed to create org membership: %w", err)
		}

		// 4. Wallet settings. No overdraft by default: an org must top up before
		//    it can spend past its balance.
		if _, err := tx.Exec(ctx,
			`INSERT INTO wallet_settings (org_id) VALUES ($1) ON CONFLICT (org_id) DO NOTHING;`,
			org.ID,
		); err != nil {
			return fmt.Errorf("failed to initialize wallet settings: %w", err)
		}

		// 5. Promotional credit, subject to abuse limits. Failing the limit check
		//    is not a signup failure: the account is created without the grant.
		eligible, err := grantEligible(ctx, tx, emailDomain, signupIP)
		if err != nil {
			return err
		}
		if eligible {
			if _, err := tx.Exec(ctx, `
				INSERT INTO wallet_ledger (org_id, delta, balance_after, kind, description, currency)
				VALUES ($1, $2, $2, $3, $4, 'USD');
			`, org.ID, signupGrant, domain.LedgerKindSignupCredit,
				fmt.Sprintf("Initial %s promotional credit", signupGrant.Display()),
			); err != nil {
				return fmt.Errorf("failed to issue initial signup credit: %w", err)
			}

			if _, err := tx.Exec(ctx, `
				INSERT INTO signup_grants (org_id, user_id, amount, email_domain, signup_ip)
				VALUES ($1, $2, $3, $4, $5);
			`, org.ID, user.ID, signupGrant, emailDomain, signupIP); err != nil {
				return fmt.Errorf("failed to record signup grant: %w", err)
			}
			grantIssued = true
		}

		return nil
	})

	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	// Generate JWT pair
	pair, err := h.tm.GenerateTokenPair(user.ID, org.ID, domain.RoleAdmin)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to generate authentication tokens")
		return
	}

	resp := AuthResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    int(h.tm.AccessExpiry().Seconds()),
		User:         user,
		Organization: org,
	}
	if grantIssued {
		resp.PromotionalCredit = signupGrant.String()
	}

	writeJSON(w, http.StatusCreated, resp)
}

// ─── signup helpers ───────────────────────────────────────────

// grantEligible reports whether a new signup should receive the promotional
// credit. Both limits are per rolling 24 hours.
func grantEligible(ctx context.Context, tx pgx.Tx, emailDomain string, signupIP *string) (bool, error) {
	if signupIP != nil {
		var ipCount int
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM signup_grants
			WHERE signup_ip = $1 AND created_at > NOW() - INTERVAL '24 hours';
		`, *signupIP).Scan(&ipCount); err != nil {
			return false, fmt.Errorf("failed to check signup grant limits: %w", err)
		}
		if ipCount >= maxGrantsPerIPPerDay {
			return false, nil
		}
	}

	var domainCount int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM signup_grants
		WHERE email_domain = $1 AND created_at > NOW() - INTERVAL '24 hours';
	`, emailDomain).Scan(&domainCount); err != nil {
		return false, fmt.Errorf("failed to check signup grant limits: %w", err)
	}
	return domainCount < maxGrantsPerDomainPerDay, nil
}

var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s.]+(\.[^@\s.]+)+$`)

func validEmail(s string) bool {
	s = strings.TrimSpace(s)
	return len(s) <= 254 && emailPattern.MatchString(s)
}

func emailDomainOf(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return "unknown"
	}
	return strings.ToLower(email[at+1:])
}

// clientIP returns the caller's address for abuse accounting. X-Forwarded-For is
// only trusted when TRUST_PROXY_HEADERS is set, since a client can forge it.
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

func normalizeCountry(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	if len(c) != 2 {
		return "US"
	}
	return c
}

// defaultRegionForCountry picks the nearest active region. It is a coarse
// mapping; customers can override it per deployment.
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

// currencyForCountry chooses the billing display currency. Internal accounting
// stays in USD; invoices convert at a recorded fx_rate.
func currencyForCountry(country string) string {
	switch country {
	case "IN":
		return "INR"
	case "GB":
		return "GBP"
	case "DE", "FR", "NL", "ES", "IT", "PL", "IE", "BE", "AT", "FI", "PT", "GR":
		return "EUR"
	case "JP":
		return "JPY"
	case "AU":
		return "AUD"
	case "CA":
		return "CAD"
	case "SG":
		return "SGD"
	case "BR":
		return "BRL"
	case "AE":
		return "AED"
	case "ZA":
		return "ZAR"
	default:
		return "USD"
	}
}

func (h *AuthHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	ctx := r.Context()
	var user domain.User
	var passwordHash string

	userQuery := `
		SELECT id, email, password_hash, name, auth_provider, email_verified, created_at, updated_at
		FROM users
		WHERE email = $1 AND deleted_at IS NULL;
	`
	err := h.db.Pool.QueryRow(ctx, userQuery, strings.ToLower(strings.TrimSpace(req.Email))).Scan(
		&user.ID, &user.Email, &passwordHash, &user.Name, &user.AuthProvider, &user.EmailVerified, &user.CreatedAt, &user.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Hash anyway so a missing account and a wrong password take the same
			// time; otherwise response latency enumerates registered emails.
			auth.DummyPasswordCheck()
			writeError(w, http.StatusUnauthorized, "Invalid email or password")
			return
		}
		writeError(w, http.StatusInternalServerError, "Database error")
		return
	}

	if !auth.CheckPasswordHash(req.Password, passwordHash) {
		writeError(w, http.StatusUnauthorized, "Invalid email or password")
		return
	}

	// Get primary organization & membership role
	var org domain.Organization
	var role string
	orgQuery := `
		SELECT o.id, o.name, o.default_region, o.created_at, o.updated_at, m.role
		FROM organizations o
		JOIN memberships m ON m.org_id = o.id
		WHERE m.user_id = $1 AND o.deleted_at IS NULL
		ORDER BY m.created_at ASC
		LIMIT 1;
	`
	err = h.db.Pool.QueryRow(ctx, orgQuery, user.ID).Scan(
		&org.ID, &org.Name, &org.DefaultRegion, &org.CreatedAt, &org.UpdatedAt, &role,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to resolve user organization")
		return
	}

	pair, err := h.tm.GenerateTokenPair(user.ID, org.ID, role)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to generate authentication tokens")
		return
	}

	writeJSON(w, http.StatusOK, AuthResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    int(h.tm.AccessExpiry().Seconds()),
		User:         user,
		Organization: org,
	})
}

// ─── Refresh, logout ──────────────────────────────────────────

type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type RefreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// HandleRefresh exchanges a refresh token for a new token pair. The presented
// refresh token is revoked in the same request, so refresh tokens rotate and a
// stolen one stops working as soon as the legitimate client refreshes.
func (h *AuthHandler) HandleRefresh(w http.ResponseWriter, r *http.Request) {
	var req RefreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RefreshToken == "" {
		writeError(w, http.StatusBadRequest, "refresh_token is required")
		return
	}

	claims, err := h.tm.VerifyRefreshToken(req.RefreshToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "Invalid or expired refresh token")
		return
	}

	ctx := r.Context()

	// Consume rather than Revoke: replaying the same refresh token must fail even
	// if two requests arrive at once.
	var expiresAt time.Time
	if claims.ExpiresAt != nil {
		expiresAt = claims.ExpiresAt.Time
	} else {
		expiresAt = time.Now().Add(h.tm.RefreshExpiry())
	}
	if err := h.revocations.Consume(ctx, claims.ID, expiresAt); err != nil {
		if errors.Is(err, auth.ErrTokenAlreadyUsed) {
			// A replayed refresh token is a strong signal of theft: drop every
			// session for the user and make them sign in again.
			_ = h.revocations.RevokeAllForUser(ctx, claims.UserID, "refresh token replay detected")
			writeError(w, http.StatusUnauthorized, "Refresh token has already been used; please sign in again")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "Unable to verify credential state")
		return
	}

	if cutoff, cErr := h.revocations.IssuedBefore(ctx, claims.UserID); cErr == nil &&
		!cutoff.IsZero() && claims.IssuedAt != nil && claims.IssuedAt.Before(cutoff) {
		writeError(w, http.StatusUnauthorized, "Credentials were invalidated; please sign in again")
		return
	}

	// Re-read the role from the database so a demotion takes effect on refresh
	// rather than persisting for the life of the refresh token.
	var role string
	err = h.db.Pool.QueryRow(ctx, `
		SELECT m.role
		FROM memberships m
		JOIN users u ON u.id = m.user_id
		WHERE m.user_id = $1 AND m.org_id = $2 AND u.deleted_at IS NULL;
	`, claims.UserID, claims.OrgID).Scan(&role)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "Membership is no longer valid")
		return
	}

	pair, err := h.tm.GenerateTokenPair(claims.UserID, claims.OrgID, role)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to generate authentication tokens")
		return
	}

	writeJSON(w, http.StatusOK, RefreshResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    int(h.tm.AccessExpiry().Seconds()),
	})
}

// HandleLogout revokes the access token used to make the request, and the
// refresh token if one is supplied.
func (h *AuthHandler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.GetClaims(r.Context())
	if !ok || claims == nil {
		writeError(w, http.StatusUnauthorized, "Unauthenticated")
		return
	}

	ctx := r.Context()
	accessExpiry := time.Now().Add(h.tm.AccessExpiry())
	if claims.ExpiresAt != nil {
		accessExpiry = claims.ExpiresAt.Time
	}
	if err := h.revocations.Revoke(ctx, claims.ID, accessExpiry, "logout"); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to revoke session")
		return
	}

	// Revoking the refresh token too is what makes logout meaningful; otherwise
	// the client could immediately mint a new access token.
	var req RefreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err == nil && req.RefreshToken != "" {
		if rc, rErr := h.tm.VerifyRefreshToken(req.RefreshToken); rErr == nil && rc.UserID == claims.UserID {
			exp := time.Now().Add(h.tm.RefreshExpiry())
			if rc.ExpiresAt != nil {
				exp = rc.ExpiresAt.Time
			}
			_ = h.revocations.Revoke(ctx, rc.ID, exp, "logout")
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

// HandleLogoutAll invalidates every token issued to the caller, which is the
// action a user needs after a suspected credential compromise.
func (h *AuthHandler) HandleLogoutAll(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.GetClaims(r.Context())
	if !ok || claims == nil {
		writeError(w, http.StatusUnauthorized, "Unauthenticated")
		return
	}

	if err := h.revocations.RevokeAllForUser(r.Context(), claims.UserID, "user requested sign-out everywhere"); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to invalidate sessions")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "all_sessions_invalidated"})
}

func (h *AuthHandler) HandleMe(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.GetClaims(r.Context())
	if !ok || claims == nil {
		writeError(w, http.StatusUnauthorized, "Unauthenticated")
		return
	}

	ctx := r.Context()
	var user domain.User
	userQuery := `SELECT id, email, name, auth_provider, email_verified, created_at, updated_at FROM users WHERE id = $1;`
	err := h.db.Pool.QueryRow(ctx, userQuery, claims.UserID).Scan(
		&user.ID, &user.Email, &user.Name, &user.AuthProvider, &user.EmailVerified, &user.CreatedAt, &user.UpdatedAt,
	)
	if err != nil {
		writeError(w, http.StatusNotFound, "User not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user":   user,
		"org_id": claims.OrgID,
		"role":   claims.Role,
	})
}

// ─── API Key Handler ──────────────────────────────────────────

type CreateAPIKeyRequest struct {
	Name string `json:"name"`
}

type CreateAPIKeyResponse struct {
	ApiKey domain.ApiKey `json:"api_key"`
	Secret string        `json:"secret"` // Raw key returned ONCE
}

func (h *AuthHandler) HandleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.GetClaims(r.Context())

	var req CreateAPIKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeError(w, http.StatusBadRequest, "API key name is required")
		return
	}

	rawKey, prefix, hash, err := auth.GenerateAPIKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to generate API key")
		return
	}

	ctx := r.Context()
	var apiKey domain.ApiKey
	query := `
		INSERT INTO api_keys (org_id, name, prefix, hash, scope)
		VALUES ($1, $2, $3, $4, 'org')
		RETURNING id, org_id, name, prefix, scope, permissions, revoked, created_at;
	`
	err = h.db.Pool.QueryRow(ctx, query, claims.OrgID, req.Name, prefix, hash).Scan(
		&apiKey.ID, &apiKey.OrgID, &apiKey.Name, &apiKey.Prefix, &apiKey.Scope, &apiKey.Permissions, &apiKey.Revoked, &apiKey.CreatedAt,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to save API key")
		return
	}

	writeJSON(w, http.StatusCreated, CreateAPIKeyResponse{
		ApiKey: apiKey,
		Secret: rawKey,
	})
}

// HandleListAPIKeys returns all non-revoked API keys for the caller's org.
func (h *AuthHandler) HandleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.GetClaims(r.Context())
	ctx := r.Context()

	query := `
		SELECT id, org_id, name, prefix, scope, permissions, last_used_at, revoked, created_at
		FROM api_keys
		WHERE org_id = $1 AND revoked = FALSE
		ORDER BY created_at DESC;
	`
	rows, err := h.db.Pool.Query(ctx, query, claims.OrgID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to query API keys")
		return
	}
	defer rows.Close()

	keys := []domain.ApiKey{}
	for rows.Next() {
		var k domain.ApiKey
		if err := rows.Scan(&k.ID, &k.OrgID, &k.Name, &k.Prefix, &k.Scope, &k.Permissions,
			&k.LastUsedAt, &k.Revoked, &k.CreatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to scan API key")
			return
		}
		keys = append(keys, k)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"api_keys": keys,
		"count":    len(keys),
	})
}

// HandleRevokeAPIKey soft-revokes an API key by its ID. Only admins can revoke.
func (h *AuthHandler) HandleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.GetClaims(r.Context())
	keyID := r.PathValue("id")
	if keyID == "" {
		writeError(w, http.StatusBadRequest, "API key ID is required")
		return
	}

	ctx := r.Context()
	tag, err := h.db.Pool.Exec(ctx,
		`UPDATE api_keys SET revoked = TRUE WHERE id = $1 AND org_id = $2 AND revoked = FALSE;`,
		keyID, claims.OrgID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to revoke API key")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "API key not found or already revoked")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked", "id": keyID})
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
