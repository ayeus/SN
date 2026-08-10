package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ayeus/ayeusann/internal/auth"
)

func TestPasswordHashing(t *testing.T) {
	t.Run("ValidPassword", func(t *testing.T) {
		password := "secretPassword123"
		hash, err := auth.HashPassword(password)
		if err != nil {
			t.Fatalf("HashPassword error: %v", err)
		}

		if !auth.CheckPasswordHash(password, hash) {
			t.Fatal("CheckPasswordHash failed for correct password")
		}

		if auth.CheckPasswordHash("wrongPassword", hash) {
			t.Fatal("CheckPasswordHash succeeded for wrong password")
		}
	})

	t.Run("ShortPassword", func(t *testing.T) {
		_, err := auth.HashPassword("short")
		if err != auth.ErrPasswordTooShort {
			t.Fatalf("Expected ErrPasswordTooShort, got %v", err)
		}
	})
}

func TestJWTTokenManager(t *testing.T) {
	tm := auth.NewTokenManager("test-secret-key-32-bytes-long!!!", 5*time.Minute, 1*time.Hour)

	userID := "usr_123"
	orgID := "org_456"
	role := "admin"

	accessToken, refreshToken, err := tm.GeneratePair(userID, orgID, role)
	if err != nil {
		t.Fatalf("GeneratePair error: %v", err)
	}

	claims, err := tm.VerifyToken(accessToken)
	if err != nil {
		t.Fatalf("VerifyToken access token error: %v", err)
	}

	if claims.UserID != userID || claims.OrgID != orgID || claims.Role != role {
		t.Fatalf("Mismatch in claims: %+v", claims)
	}

	refClaims, err := tm.VerifyToken(refreshToken)
	if err != nil {
		t.Fatalf("VerifyToken refresh token error: %v", err)
	}
	if refClaims.UserID != userID || refClaims.OrgID != orgID {
		t.Fatalf("Mismatch in refresh claims: %+v", refClaims)
	}
}

func TestAPIKeyGeneration(t *testing.T) {
	rawKey, prefix, hash, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey error: %v", err)
	}

	if len(rawKey) < 20 || rawKey[:8] != "sk_live_" {
		t.Fatalf("Invalid rawKey format: %s", rawKey)
	}

	if len(prefix) != 12 || prefix[:8] != "sk_live_" {
		t.Fatalf("Invalid prefix format: %s", prefix)
	}

	if auth.HashAPIKey(rawKey) != hash {
		t.Fatal("HashAPIKey mismatch")
	}

	extracted, err := auth.ExtractPrefix(rawKey)
	if err != nil {
		t.Fatalf("ExtractPrefix error: %v", err)
	}
	if extracted != prefix {
		t.Fatalf("Expected prefix %s, got %s", prefix, extracted)
	}
}

func TestAuthMiddlewareAndRBAC(t *testing.T) {
	tm := auth.NewTokenManager("test-secret-key-32-bytes-long!!!", 5*time.Minute, 1*time.Hour)

	adminToken, _, _ := tm.GeneratePair("usr_admin", "org_1", "admin")
	memberToken, _, _ := tm.GeneratePair("usr_member", "org_1", "member")

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, _ := auth.GetClaims(r.Context())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(claims.UserID))
	})

	protectedRoute := tm.AuthMiddleware(auth.RequireRole("admin")(handler))

	t.Run("ValidAdminAccess", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/admin/test", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		w := httptest.NewRecorder()

		protectedRoute.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("Expected status 200, got %d", w.Code)
		}
		if w.Body.String() != "usr_admin" {
			t.Fatalf("Expected body 'usr_admin', got %s", w.Body.String())
		}
	})

	t.Run("ForbiddenMemberAccessToAdminRoute", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/admin/test", nil)
		req.Header.Set("Authorization", "Bearer "+memberToken)
		w := httptest.NewRecorder()

		protectedRoute.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("Expected status 403 Forbidden, got %d", w.Code)
		}
	})

	t.Run("MissingToken", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/admin/test", nil)
		w := httptest.NewRecorder()

		protectedRoute.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("Expected status 401 Unauthorized, got %d", w.Code)
		}
	})
}
