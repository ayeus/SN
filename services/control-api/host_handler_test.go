package main

import (
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
	"github.com/ayeus/ayeusann/internal/platform"
)

func TestHostManagementEndpoints(t *testing.T) {
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

	srv, err := platform.NewServer(platform.ServiceConfig{Name: "test-control-api", Version: "0.1.0", Port: 9999})
	if err != nil {
		t.Fatalf("NewServer error: %v", err)
	}

	authHandler := NewAuthHandler(dbClient, tm, revStore, ledger)
	hostHandler := NewHostHandler(dbClient, tm)

	srv.Mux.HandleFunc("POST /v1/auth/signup", authHandler.HandleSignup)

	protectedMux := http.NewServeMux()
	protectedMux.Handle("POST /v1/hosts/register-token", auth.RequireRole("admin", "member")(http.HandlerFunc(hostHandler.HandleIssueRegistrationToken)))
	protectedMux.HandleFunc("GET /v1/hosts", hostHandler.HandleListHosts)

	srv.Mux.Handle("/", tm.AuthMiddleware(protectedMux))

	// Setup user & org for auth token
	uniqueEmail := "host-installer-" + uuid.New().String() + "@AyeusANN.io"
	signupPayload, _ := json.Marshal(map[string]string{
		"email":    uniqueEmail,
		"password": "password1234",
		"name":     "Host Installer",
	})
	signupReq := httptest.NewRequest(http.MethodPost, "/v1/auth/signup", bytes.NewBuffer(signupPayload))
	signupW := httptest.NewRecorder()
	srv.Mux.ServeHTTP(signupW, signupReq)

	var signupRes map[string]interface{}
	_ = json.Unmarshal(signupW.Body.Bytes(), &signupRes)
	accessToken := signupRes["access_token"].(string)

	// 1. Issue Registration Token
	var regToken string
	t.Run("IssueRegistrationToken", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/hosts/register-token", nil)
		req.Header.Set("Authorization", "Bearer "+accessToken)
		w := httptest.NewRecorder()

		srv.Mux.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("Expected status 201 Created, got %d. Body: %s", w.Code, w.Body.String())
		}

		var res map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &res)

		token, ok := res["registration_token"].(string)
		if !ok || token == "" {
			t.Fatal("Expected non-empty registration_token")
		}
		regToken = token
	})

	// Verify registration token can be decoded by TokenManager
	t.Run("VerifyRegistrationToken", func(t *testing.T) {
		claims, err := tm.VerifyRegistrationToken(regToken)
		if err != nil {
			t.Fatalf("Failed to verify issued registration token: %v", err)
		}
		if claims.Role != "host_installer" {
			t.Fatalf("Expected role 'host_installer', got %s", claims.Role)
		}
	})

	// 2. List Hosts (Initial empty list)
	t.Run("ListHostsEmpty", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/hosts", nil)
		req.Header.Set("Authorization", "Bearer "+accessToken)
		w := httptest.NewRecorder()

		srv.Mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("Expected status 200 OK, got %d. Body: %s", w.Code, w.Body.String())
		}
	})
}
