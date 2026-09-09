package main

import (
	"os"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/money"
	"github.com/ayeus/ayeusann/internal/platform"
)

func getTestDBURL() string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann?sslmode=disable"
}
const testJWTSecret = "test-secret-key-32-bytes-long-super-secure!"

func TestSignupLoginAndAPIKeyFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dbClient, err := db.NewClient(ctx, db.Config{URL: getTestDBURL()})
	if err != nil {
		t.Fatalf("Failed to connect to test database: %v", err)
	}
	defer dbClient.Close()

	tm, err := auth.NewTokenManager(testJWTSecret, 15*time.Minute, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("NewTokenManager error: %v", err)
	}
	revStore := auth.NewMemoryRevocationStore()
	ledger := db.NewLedgerService(dbClient)

	// Create test server mux
	srv, err := platform.NewServer(platform.ServiceConfig{Name: "test-control-api", Version: "0.1.0", Port: 9999})
	if err != nil {
		t.Fatalf("NewServer error: %v", err)
	}

	authHandler := NewAuthHandler(dbClient, tm, revStore, ledger)

	srv.Mux.HandleFunc("POST /v1/auth/signup", authHandler.HandleSignup)
	srv.Mux.HandleFunc("POST /v1/auth/login", authHandler.HandleLogin)

	protectedMux := http.NewServeMux()
	protectedMux.HandleFunc("GET /v1/auth/me", authHandler.HandleMe)
	protectedMux.Handle("POST /v1/api-keys", auth.RequireRole("admin", "member")(http.HandlerFunc(authHandler.HandleCreateAPIKey)))

	srv.Mux.Handle("/", tm.AuthMiddleware(protectedMux))

	uniqueEmail := "dev-" + uuid.New().String() + "@AyeusANN.io"
	password := "securePassword123"

	// 1. Test Signup
	t.Run("Signup", func(t *testing.T) {
		payload, _ := json.Marshal(map[string]string{
			"email":    uniqueEmail,
			"password": password,
			"name":     "Dev Engineer",
		})

		req := httptest.NewRequest(http.MethodPost, "/v1/auth/signup", bytes.NewBuffer(payload))
		w := httptest.NewRecorder()

		srv.Mux.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("Expected status 201 Created, got %d. Body: %s", w.Code, w.Body.String())
		}

		var res map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &res)

		if res["access_token"] == nil || res["user"] == nil || res["organization"] == nil {
			t.Fatalf("Response missing required fields: %+v", res)
		}

		// Verify signup credit ($50 balance)
		orgMap := res["organization"].(map[string]interface{})
		orgID := orgMap["id"].(string)
		bal, err := ledger.GetBalance(ctx, orgID)
		if err != nil || bal != money.MustParse("50.00", "USD") {
			t.Fatalf("Expected signup credit balance 50.00, got %s (err: %v)", bal.String(), err)
		}
	})

	// 2. Test Login & Get Token
	var accessToken string
	t.Run("Login", func(t *testing.T) {
		payload, _ := json.Marshal(map[string]string{
			"email":    uniqueEmail,
			"password": password,
		})

		req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewBuffer(payload))
		w := httptest.NewRecorder()

		srv.Mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("Expected status 200 OK, got %d. Body: %s", w.Code, w.Body.String())
		}

		var res map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &res)

		token, ok := res["access_token"].(string)
		if !ok || token == "" {
			t.Fatal("Expected non-empty access_token")
		}
		accessToken = token
	})

	// 3. Test Protected GET /v1/auth/me
	t.Run("GetProfileMe", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/auth/me", nil)
		req.Header.Set("Authorization", "Bearer "+accessToken)
		w := httptest.NewRecorder()

		srv.Mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("Expected status 200 OK, got %d. Body: %s", w.Code, w.Body.String())
		}

		var res map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		if res["role"] != "admin" {
			t.Fatalf("Expected role 'admin', got %v", res["role"])
		}
	})

	// 4. Test Create API Key
	t.Run("CreateAPIKey", func(t *testing.T) {
		payload, _ := json.Marshal(map[string]string{
			"name": "Production Key",
		})

		req := httptest.NewRequest(http.MethodPost, "/v1/api-keys", bytes.NewBuffer(payload))
		req.Header.Set("Authorization", "Bearer "+accessToken)
		w := httptest.NewRecorder()

		srv.Mux.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("Expected status 201 Created, got %d. Body: %s", w.Code, w.Body.String())
		}

		var res map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &res)

		secret, ok := res["secret"].(string)
		if !ok || secret[:8] != "sk_live_" {
			t.Fatalf("Expected raw secret starting with 'sk_live_', got %s", secret)
		}
	})
}
