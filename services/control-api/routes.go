package main

import (
	"net/http"

	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/httpx"
)

// Routes builds the control-plane HTTP surface (Implementation Guide §4).
// Tests exercise this exact handler, so the wiring they cover is the wiring
// that ships.
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	authn := auth.NewMiddleware(a.tm, a.revocations)
	idem := httpx.NewIdempotency(a.db.Pool, func(r *http.Request) string {
		if c := claimsOf(r); c != nil {
			return c.OrgID
		}
		return ""
	})

	// protect authenticates, then applies Idempotency-Key replay (IR-1).
	protect := func(h http.Handler) http.Handler { return authn.Authenticate(idem.Wrap(h)) }
	role := func(roles ...string) func(http.HandlerFunc) http.Handler {
		return func(h http.HandlerFunc) http.Handler { return protect(auth.RequireRole(roles...)(h)) }
	}
	member := role("admin", "member")
	orgAdmin := role("admin")
	anyRole := func(h http.HandlerFunc) http.Handler { return protect(h) }
	optional := func(h http.HandlerFunc) http.Handler { return a.optionalAuth(h) }

	// ── Public ───────────────────────────────────────────────
	mux.HandleFunc("POST /v1/auth/signup", a.HandleSignup)
	mux.HandleFunc("POST /v1/auth/login", a.HandleLogin)
	mux.HandleFunc("POST /v1/auth/refresh", a.HandleRefresh)
	mux.HandleFunc("POST /v1/auth/password/forgot", a.HandleForgotPassword)
	mux.HandleFunc("POST /v1/auth/password/reset", a.HandleResetPassword)
	mux.Handle("GET /v1/models", optional(a.HandleListModels))
	mux.Handle("GET /v1/models/{id}", optional(a.HandleGetModel))
	mux.HandleFunc("GET /v1/regions", a.HandleRegions)
	mux.HandleFunc("GET /v1/network/stats", a.HandleNetworkStats)
	mux.HandleFunc("GET /v1/network/address", a.HandleNetworkAddress)

	// ── Session and organisation ─────────────────────────────
	mux.Handle("GET /v1/auth/me", anyRole(a.HandleMe))
	mux.Handle("POST /v1/auth/logout", anyRole(a.HandleLogout))
	mux.Handle("POST /v1/auth/logout-all", anyRole(a.HandleLogoutAll))
	mux.Handle("GET /v1/orgs/{id}", protect(auth.RequireOrg("id")(http.HandlerFunc(a.HandleGetOrg))))
	mux.Handle("PATCH /v1/orgs/{id}", protect(auth.RequireOrg("id")(auth.RequireRole("admin")(http.HandlerFunc(a.HandleUpdateOrg)))))

	// ── API keys (SRS FR-3, FR-5) ────────────────────────────
	mux.Handle("POST /v1/api-keys", member(a.HandleCreateAPIKey))
	mux.Handle("GET /v1/api-keys", member(a.HandleListAPIKeys))
	mux.Handle("DELETE /v1/api-keys/{id}", orgAdmin(a.HandleRevokeAPIKey))

	// ── Models and deployments ───────────────────────────────
	mux.Handle("POST /v1/models/byo", member(a.HandleBYOModel))
	mux.Handle("GET /v1/capacity", anyRole(a.HandleCapacity))
	mux.Handle("POST /v1/deployments", member(a.HandleCreateDeployment))
	mux.Handle("GET /v1/deployments", anyRole(a.HandleListDeployments))
	mux.Handle("GET /v1/deployments/{id}", anyRole(a.HandleGetDeployment))
	mux.Handle("PATCH /v1/deployments/{id}", member(a.HandleUpdateDeployment))
	mux.Handle("DELETE /v1/deployments/{id}", member(a.HandleStopDeployment))
	mux.Handle("GET /v1/deployments/{id}/replicas", anyRole(a.HandleDeploymentReplicas))
	mux.Handle("GET /v1/deployments/{id}/metrics", anyRole(a.HandleDeploymentMetrics))
	mux.Handle("GET /v1/deployments/{id}/logs", anyRole(a.HandleDeploymentLogs))
	mux.Handle("GET /v1/usage/daily", anyRole(a.HandleUsageDaily))

	// ── Hosts (supply side) ──────────────────────────────────
	mux.Handle("POST /v1/hosts/register-token", member(a.HandleIssueRegistrationToken))
	mux.Handle("GET /v1/host-tokens/{id}", anyRole(a.HandleRegistrationTokenStatus))
	mux.Handle("GET /v1/hosts", anyRole(a.HandleListHosts))
	mux.Handle("GET /v1/hosts/activity", anyRole(a.HandleAllHostActivity))
	mux.Handle("GET /v1/hosts/{id}", anyRole(a.HandleGetHost))
	mux.Handle("GET /v1/hosts/{id}/activity", anyRole(a.HandleHostActivity))
	mux.Handle("GET /v1/hosts/{id}/telemetry", anyRole(a.HandleHostTelemetry))
	mux.Handle("PATCH /v1/hosts/{id}/controls", anyRole(a.HandleHostControls))
	mux.Handle("DELETE /v1/hosts/{id}", anyRole(a.HandleDecommissionHost))

	// ── Ops console (SRS FR-83) ──────────────────────────────
	admin := func(h http.HandlerFunc) http.Handler { return protect(a.requirePlatformAdmin(h)) }
	mux.Handle("GET /v1/admin/fleet", admin(a.HandleAdminFleet))
	mux.Handle("GET /v1/admin/incidents", admin(a.HandleAdminIncidents))
	mux.Handle("POST /v1/admin/hosts/{id}/drain", admin(a.HandleAdminDrainHost))
	mux.Handle("POST /v1/admin/hosts/{id}/ban", admin(a.HandleAdminBanHost))
	mux.Handle("POST /v1/admin/hosts/{id}/tier", admin(a.HandleAdminSetHostTier))
	mux.Handle("POST /v1/admin/deployments/{id}/kill", admin(a.HandleAdminKillDeployment))

	return platformMetrics(mux)
}

// optionalAuth attaches claims when a valid session is present but never
// rejects: the catalogue is public, and a signed-in caller also sees their own
// BYO models.
func (a *API) optionalAuth(next http.HandlerFunc) http.Handler {
	authn := auth.NewMiddleware(a.tm, a.revocations)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			next(w, r)
			return
		}
		probe := &statusProbe{ResponseWriter: w}
		authn.Authenticate(next).ServeHTTP(probe, r)
		if probe.rejected {
			// Invalid token on a public route: serve it anonymously.
			r.Header.Del("Authorization")
			next(w, r)
		}
	})
}

// statusProbe swallows a 401 from the auth middleware so optionalAuth can fall
// back to anonymous access.
type statusProbe struct {
	http.ResponseWriter
	rejected bool
	wrote    bool
}

func (p *statusProbe) WriteHeader(code int) {
	if !p.wrote && code == http.StatusUnauthorized {
		p.rejected = true
		return
	}
	p.wrote = true
	p.ResponseWriter.WriteHeader(code)
}

func (p *statusProbe) Write(b []byte) (int, error) {
	if p.rejected {
		return len(b), nil
	}
	p.wrote = true
	return p.ResponseWriter.Write(b)
}
