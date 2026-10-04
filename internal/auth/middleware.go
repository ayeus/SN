package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

type contextKey string

const (
	UserContextKey = contextKey("user_claims")
)

// Middleware authenticates requests using access tokens and enforces revocation.
type Middleware struct {
	tm         *TokenManager
	revocation RevocationStore
}

// NewMiddleware creates an authentication middleware. The revocation store may
// be nil only in tests; production callers must supply one so that logout and
// credential rotation actually take effect.
func NewMiddleware(tm *TokenManager, revocation RevocationStore) *Middleware {
	return &Middleware{tm: tm, revocation: revocation}
}

// Authenticate verifies the Bearer access token and attaches Claims to the request
// context. It rejects refresh tokens, host registration tokens, revoked tokens,
// and tokens issued before a user-wide invalidation.
func (m *Middleware) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "Missing or malformed Authorization header, expected 'Bearer <token>'")
			return
		}

		// Requires audience ayeusann-api, so a refresh or registration token
		// presented here is rejected rather than silently accepted.
		claims, err := m.tm.VerifyAccessToken(token)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "Invalid or expired token")
			return
		}

		if m.revocation != nil {
			revoked, rErr := m.revocation.IsRevoked(r.Context(), claims.ID)
			if rErr != nil {
				// Fail closed: an unavailable revocation store must not become
				// a way to keep using a logged-out credential.
				writeJSONError(w, http.StatusServiceUnavailable, "Unable to verify credential state")
				return
			}
			if revoked {
				writeJSONError(w, http.StatusUnauthorized, "Token has been revoked")
				return
			}

			cutoff, cErr := m.revocation.IssuedBefore(r.Context(), claims.UserID)
			if cErr == nil && !cutoff.IsZero() && claims.IssuedAt != nil && claims.IssuedAt.Before(cutoff) {
				writeJSONError(w, http.StatusUnauthorized, "Credentials were invalidated; please sign in again")
				return
			}
		}

		ctx := context.WithValue(r.Context(), UserContextKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AuthMiddleware is retained for compatibility with existing call sites.
//
// Deprecated: prefer NewMiddleware(...).Authenticate, which enforces token
// revocation. This method cannot check revocation because a TokenManager has no
// store attached.
func (tm *TokenManager) AuthMiddleware(next http.Handler) http.Handler {
	return NewMiddleware(tm, nil).Authenticate(next)
}

// RequireRole enforces role-based access control on routes.
// Roles: "admin", "member", "billing". An admin satisfies any role requirement.
// The host_installer role never satisfies an API role requirement — those tokens
// exist only to enrol a host.
func RequireRole(allowedRoles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := GetClaims(r.Context())
			if !ok || claims == nil {
				writeJSONError(w, http.StatusUnauthorized, "Unauthenticated")
				return
			}

			if claims.Role == RoleHostInstaller {
				writeJSONError(w, http.StatusForbidden, "Host registration credentials cannot access this API")
				return
			}

			// Admin is a superuser within its own org.
			if strings.EqualFold(claims.Role, "admin") {
				next.ServeHTTP(w, r)
				return
			}

			for _, role := range allowedRoles {
				if strings.EqualFold(claims.Role, role) {
					next.ServeHTTP(w, r)
					return
				}
			}

			writeJSONError(w, http.StatusForbidden, "Forbidden: insufficient organization permissions")
		})
	}
}

// RequireOrg ensures the authenticated caller belongs to an organization and that
// it matches the org_id in the request path. This is the check that stops one
// tenant from reading another tenant's organisation or usage.
func RequireOrg(pathValue string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := GetClaims(r.Context())
			if !ok || claims == nil {
				writeJSONError(w, http.StatusUnauthorized, "Unauthenticated")
				return
			}
			if claims.OrgID == "" {
				writeJSONError(w, http.StatusForbidden, "Token is not scoped to an organization")
				return
			}

			requested := r.PathValue(pathValue)
			if requested != "" && requested != claims.OrgID {
				// Deliberately 404 rather than 403: confirming that an org id
				// exists is itself a small information leak.
				writeJSONError(w, http.StatusNotFound, "Not found")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// GetClaims retrieves Claims from the request context.
func GetClaims(ctx context.Context) (*Claims, bool) {
	claims, ok := ctx.Value(UserContextKey).(*Claims)
	return claims, ok
}

// bearerToken extracts a Bearer token from the Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return "", false
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", false
	}
	return token, true
}

func writeJSONError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": message,
	})
}
