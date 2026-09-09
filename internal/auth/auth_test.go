package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ayeus/ayeusann/internal/auth"
)

const testSecret = "test-secret-key-32-bytes-long!!!!"

func newTM(t *testing.T) *auth.TokenManager {
	t.Helper()
	tm, err := auth.NewTokenManager(testSecret, 5*time.Minute, 1*time.Hour)
	if err != nil {
		t.Fatalf("NewTokenManager error: %v", err)
	}
	return tm
}

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

	t.Run("CommonPasswordRejected", func(t *testing.T) {
		if _, err := auth.HashPassword("password123"); err != auth.ErrPasswordCommon {
			t.Fatalf("Expected ErrPasswordCommon, got %v", err)
		}
	})

	t.Run("SingleCharacterClassRejected", func(t *testing.T) {
		if _, err := auth.HashPassword("abcdefghijklmnop"); err != auth.ErrPasswordTooSimple {
			t.Fatalf("Expected ErrPasswordTooSimple, got %v", err)
		}
	})

	t.Run("OverLongPasswordRejected", func(t *testing.T) {
		// bcrypt silently truncates past 72 bytes; accepting longer input would
		// make two distinct passwords interchangeable.
		long := make([]byte, 80)
		for i := range long {
			long[i] = 'a'
		}
		if _, err := auth.HashPassword(string(long)); err != auth.ErrPasswordTooLong {
			t.Fatalf("Expected ErrPasswordTooLong, got %v", err)
		}
	})
}

func TestNewTokenManagerRejectsShortSecret(t *testing.T) {
	if _, err := auth.NewTokenManager("too-short", time.Minute, time.Hour); err == nil {
		t.Fatal("expected an error for a secret shorter than 32 bytes")
	}
}

func TestJWTTokenManager(t *testing.T) {
	tm := newTM(t)

	userID := "usr_123"
	orgID := "org_456"
	role := "admin"

	pair, err := tm.GenerateTokenPair(userID, orgID, role)
	if err != nil {
		t.Fatalf("GenerateTokenPair error: %v", err)
	}

	claims, err := tm.VerifyAccessToken(pair.AccessToken)
	if err != nil {
		t.Fatalf("VerifyAccessToken error: %v", err)
	}
	if claims.UserID != userID || claims.OrgID != orgID || claims.Role != role {
		t.Fatalf("Mismatch in claims: %+v", claims)
	}
	if claims.ID == "" {
		t.Fatal("access token has no jti; it could not be revoked")
	}

	refClaims, err := tm.VerifyRefreshToken(pair.RefreshToken)
	if err != nil {
		t.Fatalf("VerifyRefreshToken error: %v", err)
	}
	if refClaims.UserID != userID || refClaims.OrgID != orgID {
		t.Fatalf("Mismatch in refresh claims: %+v", refClaims)
	}
	if refClaims.ID == claims.ID {
		t.Fatal("access and refresh tokens share a jti; revoking one would revoke both")
	}
}

// TestAudienceSeparation is the regression test for the original bug: VerifyToken
// took no audience, so a refresh token was accepted as an API credential.
func TestAudienceSeparation(t *testing.T) {
	tm := newTM(t)

	pair, err := tm.GenerateTokenPair("usr_1", "org_1", "admin")
	if err != nil {
		t.Fatalf("GenerateTokenPair error: %v", err)
	}

	if _, err := tm.VerifyAccessToken(pair.RefreshToken); err == nil {
		t.Error("a refresh token was accepted as an access token")
	}
	if _, err := tm.VerifyRefreshToken(pair.AccessToken); err == nil {
		t.Error("an access token was accepted as a refresh token")
	}

	reg, err := tm.GenerateRegistrationToken("usr_1", "org_1")
	if err != nil {
		t.Fatalf("GenerateRegistrationToken error: %v", err)
	}
	if _, err := tm.VerifyAccessToken(reg.Token); err == nil {
		t.Error("a host registration token was accepted as an access token")
	}
	if _, err := tm.VerifyRegistrationToken(reg.Token); err != nil {
		t.Errorf("registration token failed its own audience check: %v", err)
	}
	if time.Until(reg.ExpiresAt) < 23*time.Hour {
		t.Errorf("registration token expires at %v, expected ~24h out", reg.ExpiresAt)
	}
}

func TestTokenSignedWithAnotherSecretIsRejected(t *testing.T) {
	tm := newTM(t)
	other, err := auth.NewTokenManager("a-completely-different-32-byte-key!", 5*time.Minute, time.Hour)
	if err != nil {
		t.Fatalf("NewTokenManager error: %v", err)
	}

	pair, err := other.GenerateTokenPair("usr_1", "org_1", "admin")
	if err != nil {
		t.Fatalf("GenerateTokenPair error: %v", err)
	}
	if _, err := tm.VerifyAccessToken(pair.AccessToken); err == nil {
		t.Fatal("a token signed with a different secret was accepted")
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
	tm := newTM(t)
	store := auth.NewMemoryRevocationStore()
	mw := auth.NewMiddleware(tm, store)

	adminPair, err := tm.GenerateTokenPair("usr_admin", "org_1", "admin")
	if err != nil {
		t.Fatalf("GenerateTokenPair error: %v", err)
	}
	memberPair, err := tm.GenerateTokenPair("usr_member", "org_1", "member")
	if err != nil {
		t.Fatalf("GenerateTokenPair error: %v", err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, _ := auth.GetClaims(r.Context())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(claims.UserID))
	})

	protectedRoute := mw.Authenticate(auth.RequireRole("admin")(handler))

	t.Run("ValidAdminAccess", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/admin/test", nil)
		req.Header.Set("Authorization", "Bearer "+adminPair.AccessToken)
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
		req.Header.Set("Authorization", "Bearer "+memberPair.AccessToken)
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

	t.Run("RevokedTokenRejected", func(t *testing.T) {
		pair, err := tm.GenerateTokenPair("usr_admin2", "org_1", "admin")
		if err != nil {
			t.Fatalf("GenerateTokenPair error: %v", err)
		}
		claims, err := tm.VerifyAccessToken(pair.AccessToken)
		if err != nil {
			t.Fatalf("VerifyAccessToken error: %v", err)
		}
		if err := store.Revoke(context.Background(), claims.ID, time.Now().Add(time.Hour), "logout"); err != nil {
			t.Fatalf("Revoke error: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/admin/test", nil)
		req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
		w := httptest.NewRecorder()

		protectedRoute.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("Expected 401 for a revoked token, got %d", w.Code)
		}
	})

	t.Run("HostInstallerCannotAccessAPI", func(t *testing.T) {
		reg, err := tm.GenerateRegistrationToken("usr_host", "org_1")
		if err != nil {
			t.Fatalf("GenerateRegistrationToken error: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/admin/test", nil)
		req.Header.Set("Authorization", "Bearer "+reg.Token)
		w := httptest.NewRecorder()

		protectedRoute.ServeHTTP(w, req)

		// Rejected at authentication, because the audience does not match.
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("Expected 401 for a registration token on an API route, got %d", w.Code)
		}
	})
}

func TestSingleUseTokenConsumption(t *testing.T) {
	store := auth.NewMemoryRevocationStore()
	ctx := context.Background()
	exp := time.Now().Add(time.Hour)

	if err := store.Consume(ctx, "jti-1", exp); err != nil {
		t.Fatalf("first Consume failed: %v", err)
	}
	if err := store.Consume(ctx, "jti-1", exp); err != auth.ErrTokenAlreadyUsed {
		t.Fatalf("expected ErrTokenAlreadyUsed on replay, got %v", err)
	}
}

func TestRequireOrgBlocksCrossTenantAccess(t *testing.T) {
	tm := newTM(t)
	mw := auth.NewMiddleware(tm, auth.NewMemoryRevocationStore())

	pair, err := tm.GenerateTokenPair("usr_1", "org_own", "admin")
	if err != nil {
		t.Fatalf("GenerateTokenPair error: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /v1/orgs/{org_id}/balance", auth.RequireOrg("org_id")(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	))
	srv := mw.Authenticate(mux)

	t.Run("OwnOrgAllowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/orgs/org_own/balance", nil)
		req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("Expected 200 for own org, got %d", w.Code)
		}
	})

	t.Run("OtherOrgNotFound", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/orgs/org_other/balance", nil)
		req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("Expected 404 for another tenant's org, got %d", w.Code)
		}
	})
}
