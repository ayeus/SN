package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/domain"
)

type AuthHandler struct {
	db     *db.Client
	tm     *auth.TokenManager
	ledger *db.LedgerService
}

func NewAuthHandler(database *db.Client, tm *auth.TokenManager, ledger *db.LedgerService) *AuthHandler {
	return &AuthHandler{
		db:     database,
		tm:     tm,
		ledger: ledger,
	}
}

type SignupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
	OrgName  string `json:"org_name,omitempty"`
}

type AuthResponse struct {
	AccessToken  string               `json:"access_token"`
	RefreshToken string               `json:"refresh_token"`
	User         domain.User          `json:"user"`
	Organization domain.Organization  `json:"organization"`
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

	passwordHash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.OrgName == "" {
		req.OrgName = req.Name + "'s Org"
	}

	ctx := r.Context()
	var user domain.User
	var org domain.Organization

	// Transactionally create User + Organization + Membership (Admin) + Signup Credit ($50)
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
			INSERT INTO organizations (name, default_region)
			VALUES ($1, $2)
			RETURNING id, name, default_region, created_at, updated_at;
		`
		err = tx.QueryRow(ctx, orgQuery, req.OrgName, domain.RegionInSouth).Scan(
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

		// 4. Initial Signup Credit ($50.00)
		ledgerQuery := `
			INSERT INTO wallet_ledger (org_id, delta, balance_after, kind, description)
			VALUES ($1, 50.00, 50.00, $2, 'Initial $50 promotional credit');
		`
		_, err = tx.Exec(ctx, ledgerQuery, org.ID, domain.LedgerKindSignupCredit)
		if err != nil {
			return fmt.Errorf("failed to issue initial signup credit: %w", err)
		}

		return nil
	})

	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	// Generate JWT pair
	access, refresh, err := h.tm.GeneratePair(user.ID, org.ID, domain.RoleAdmin)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to generate authentication tokens")
		return
	}

	writeJSON(w, http.StatusCreated, AuthResponse{
		AccessToken:  access,
		RefreshToken: refresh,
		User:         user,
		Organization: org,
	})
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
	err := h.db.Pool.QueryRow(ctx, userQuery, req.Email).Scan(
		&user.ID, &user.Email, &passwordHash, &user.Name, &user.AuthProvider, &user.EmailVerified, &user.CreatedAt, &user.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
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

	access, refresh, err := h.tm.GeneratePair(user.ID, org.ID, role)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to generate authentication tokens")
		return
	}

	writeJSON(w, http.StatusOK, AuthResponse{
		AccessToken:  access,
		RefreshToken: refresh,
		User:         user,
		Organization: org,
	})
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
