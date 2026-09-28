package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/httpx"
	"github.com/ayeus/ayeusann/internal/money"
	"github.com/jackc/pgx/v5/pgconn"
)

// API holds the control plane's dependencies. Every handler is a method on it.
type API struct {
	db          *db.Client
	tm          *auth.TokenManager
	revocations auth.RevocationStore
	ledger      *db.LedgerService
	log         *slog.Logger

	publicURL            string // gateway base URL shown to customers
	coordinatorPublicURL string // where agents dial the coordinator
	inferenceHost        string // optional {dep-id}.<host> endpoints (SRS FR-22)
	heartbeatTimeout     time.Duration
	platformAdmins       map[string]bool
	signupCredit         money.Amount // in its own currency; converted per org
}

// Config is the environment-derived part of API.
type Config struct {
	PublicURL            string
	CoordinatorPublicURL string
	InferenceHost        string
	HeartbeatTimeout     time.Duration
	PlatformAdminEmails  []string
	SignupCredit         money.Amount
}

// NewAPI wires the control plane.
func NewAPI(database *db.Client, tm *auth.TokenManager, rev auth.RevocationStore, log *slog.Logger, cfg Config) *API {
	admins := map[string]bool{}
	for _, e := range cfg.PlatformAdminEmails {
		admins[strings.ToLower(strings.TrimSpace(e))] = true
	}
	if cfg.HeartbeatTimeout <= 0 {
		cfg.HeartbeatTimeout = 15 * time.Second
	}
	return &API{
		db:                   database,
		tm:                   tm,
		revocations:          rev,
		ledger:               db.NewLedgerService(database),
		log:                  log,
		publicURL:            strings.TrimRight(cfg.PublicURL, "/"),
		coordinatorPublicURL: cfg.CoordinatorPublicURL,
		inferenceHost:        cfg.InferenceHost,
		heartbeatTimeout:     cfg.HeartbeatTimeout,
		platformAdmins:       admins,
		signupCredit:         cfg.SignupCredit,
	}
}

// writeJSON and writeError keep handler code short.
func writeJSON(w http.ResponseWriter, status int, v any) { httpx.WriteJSON(w, status, v) }
func writeError(w http.ResponseWriter, status int, msg string) {
	httpx.WriteProblem(w, status, msg)
}

// claimsOf returns the authenticated caller. Protected routes always have one.
func claimsOf(r *http.Request) *auth.Claims {
	c, _ := auth.GetClaims(r.Context())
	return c
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isPlatformAdmin reports whether the caller operates the platform (the ops
// persona in PRD §4, P8). Org admins are not platform admins.
func (a *API) isPlatformAdmin(ctx context.Context, userID string) bool {
	if len(a.platformAdmins) == 0 || userID == "" {
		return false
	}
	var email string
	if err := a.db.Pool.QueryRow(ctx, `SELECT email FROM users WHERE id = $1 AND deleted_at IS NULL;`, userID).Scan(&email); err != nil {
		return false
	}
	return a.platformAdmins[strings.ToLower(email)]
}

// requirePlatformAdmin guards the ops console routes (SRS FR-83).
func (a *API) requirePlatformAdmin(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := claimsOf(r)
		if c == nil || !a.isPlatformAdmin(r.Context(), c.UserID) {
			writeError(w, http.StatusNotFound, "Not found")
			return
		}
		next(w, r)
	})
}

// endpointFor is the base URL a customer points an OpenAI client at.
func (a *API) endpointFor(deploymentID string) string {
	if a.inferenceHost != "" {
		return "https://" + deploymentID + "." + a.inferenceHost + "/v1"
	}
	return a.publicURL + "/v1"
}
