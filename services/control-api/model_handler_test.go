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
	"github.com/spazor/spazenode/internal/auth"
	"github.com/spazor/spazenode/internal/db"
	"github.com/spazor/spazenode/internal/domain"
	"github.com/spazor/spazenode/internal/platform"
)

func TestModelCatalogAndBYO(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dbClient, err := db.NewClient(ctx, db.Config{URL: testDBURL})
	if err != nil {
		t.Fatalf("Failed to connect to test database: %v", err)
	}
	defer dbClient.Close()

	tm := auth.NewTokenManager(testJWTSecret, 15*time.Minute, 7*24*time.Hour)
	ledger := db.NewLedgerService(dbClient)

	srv, err := platform.NewServer(platform.ServiceConfig{Name: "test-control-api", Version: "0.1.0", Port: 9999})
	if err != nil {
		t.Fatalf("NewServer error: %v", err)
	}

	authHandler := NewAuthHandler(dbClient, tm, ledger)
	modelHandler := NewModelHandler(dbClient)

	srv.Mux.HandleFunc("POST /v1/auth/signup", authHandler.HandleSignup)
	srv.Mux.HandleFunc("GET /v1/models", modelHandler.HandleListModels)
	srv.Mux.HandleFunc("GET /v1/models/", modelHandler.HandleGetModel)

	protectedMux := http.NewServeMux()
	protectedMux.Handle("POST /v1/models/byo", auth.RequireRole("admin", "member")(http.HandlerFunc(modelHandler.HandleBYOModel)))

	srv.Mux.Handle("/", tm.AuthMiddleware(protectedMux))

	// Setup user & org for auth token
	uniqueEmail := "byo-tester-" + uuid.New().String() + "@spazenode.io"
	signupPayload, _ := json.Marshal(map[string]string{
		"email":    uniqueEmail,
		"password": "password1234",
		"name":     "BYO Tester",
	})
	signupReq := httptest.NewRequest(http.MethodPost, "/v1/auth/signup", bytes.NewBuffer(signupPayload))
	signupW := httptest.NewRecorder()
	srv.Mux.ServeHTTP(signupW, signupReq)

	var signupRes map[string]interface{}
	_ = json.Unmarshal(signupW.Body.Bytes(), &signupRes)
	accessToken := signupRes["access_token"].(string)

	// 1. List Models (Verify seeded Llama 3.1 8B Instruct model)
	t.Run("ListModels", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		w := httptest.NewRecorder()

		srv.Mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("Expected status 200 OK, got %d. Body: %s", w.Code, w.Body.String())
		}

		var res map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &res)

		models := res["models"].([]interface{})
		if len(models) == 0 {
			t.Fatal("Expected at least 1 model in catalog (seeded Llama model)")
		}
	})

	// 2. Filter Models by Tier & Min VRAM
	t.Run("ListModelsWithFilter", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models?tier=t1&min_vram_gb=24", nil)
		w := httptest.NewRecorder()

		srv.Mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("Expected status 200 OK, got %d", w.Code)
		}
	})

	// 3. Get Model Detail by Name
	t.Run("GetModelDetailByName", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models/llama-3.1-8b-instruct", nil)
		w := httptest.NewRecorder()

		srv.Mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("Expected status 200 OK, got %d. Body: %s", w.Code, w.Body.String())
		}

		var res map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &res)

		m := res["model"].(map[string]interface{})
		if m["name"] != "llama-3.1-8b-instruct" {
			t.Fatalf("Expected model name 'llama-3.1-8b-instruct', got %v", m["name"])
		}

		artifacts := res["artifacts"].([]interface{})
		if len(artifacts) == 0 {
			t.Fatal("Expected model artifacts array to be populated")
		}
	})

	// 4. Create Custom BYO Model (Valid T1, T2)
	t.Run("CreateBYOModelValid", func(t *testing.T) {
		byoPayload, _ := json.Marshal(map[string]interface{}{
			"name":            "custom-finetune-" + uuid.New().String()[:8],
			"family":          "mistral",
			"params_b":        7.0,
			"license":         "apache-2.0",
			"min_vram_gb":     16,
			"tiers_allowed":   []string{domain.TierT1, domain.TierT2},
			"huggingface_url": "https://huggingface.co/org/custom-model",
			"price_in_per_1m":  0.10,
			"price_out_per_1m": 0.30,
		})

		req := httptest.NewRequest(http.MethodPost, "/v1/models/byo", bytes.NewBuffer(byoPayload))
		req.Header.Set("Authorization", "Bearer "+accessToken)
		w := httptest.NewRecorder()

		srv.Mux.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("Expected status 201 Created, got %d. Body: %s", w.Code, w.Body.String())
		}

		var m domain.Model
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		if !m.IsBYO {
			t.Fatal("Expected IsBYO to be true for registered BYO model")
		}
	})

	// 5. Create Custom BYO Model on T3 -> Reject with Security Policy Violation (ADR-007)
	t.Run("CreateBYOModelT3SecurityPolicyViolation", func(t *testing.T) {
		byoPayload, _ := json.Marshal(map[string]interface{}{
			"name":          "forbidden-t3-model-" + uuid.New().String()[:8],
			"family":        "llama",
			"params_b":      8.0,
			"license":       "mit",
			"min_vram_gb":   16,
			"tiers_allowed": []string{domain.TierT3}, // Forbidden!
		})

		req := httptest.NewRequest(http.MethodPost, "/v1/models/byo", bytes.NewBuffer(byoPayload))
		req.Header.Set("Authorization", "Bearer "+accessToken)
		w := httptest.NewRecorder()

		srv.Mux.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("Expected status 403 Forbidden for BYO model on T3, got %d. Body: %s", w.Code, w.Body.String())
		}
	})
}
