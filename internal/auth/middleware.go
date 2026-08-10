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

// AuthMiddleware extracts Bearer JWT token from Authorization header, verifies it, and attaches Claims to Context.
func (tm *TokenManager) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			writeJSONError(w, http.StatusUnauthorized, "Missing Authorization header")
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			writeJSONError(w, http.StatusUnauthorized, "Invalid Authorization format, expected 'Bearer <token>'")
			return
		}

		tokenString := parts[1]
		claims, err := tm.VerifyToken(tokenString)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "Invalid or expired token")
			return
		}

		ctx := context.WithValue(r.Context(), UserContextKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireRole enforces role-based access control (RBAC) on routes.
// Roles allowed: "admin", "member", "billing".
func RequireRole(allowedRoles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := GetClaims(r.Context())
			if !ok || claims == nil {
				writeJSONError(w, http.StatusUnauthorized, "Unauthenticated")
				return
			}

			userRole := claims.Role
			allowed := false
			for _, r := range allowedRoles {
				if strings.EqualFold(userRole, r) || userRole == "admin" { // Admin has superuser override within org
					allowed = true
					break
				}
			}

			if !allowed {
				writeJSONError(w, http.StatusForbidden, "Forbidden: insufficient organization permissions")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// GetClaims retrieves Claims from request Context.
func GetClaims(ctx context.Context) (*Claims, bool) {
	claims, ok := ctx.Value(UserContextKey).(*Claims)
	return claims, ok
}

func writeJSONError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": message,
	})
}
