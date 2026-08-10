package platform_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spazor/spazenode/internal/platform"
)

func TestNewServer(t *testing.T) {
	srv, err := platform.NewServer(platform.ServiceConfig{
		Name:    "test-service",
		Version: "0.0.1",
		Port:    9999,
	})
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	if srv == nil {
		t.Fatal("NewServer() returned nil")
	}
	if srv.Config.Name != "test-service" {
		t.Errorf("expected service name 'test-service', got %q", srv.Config.Name)
	}
}

func TestHealthzNotReady(t *testing.T) {
	srv, err := platform.NewServer(platform.ServiceConfig{
		Name:    "test-service",
		Version: "0.0.1",
		Port:    9999,
	})
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	srv.Mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503 (not ready), got %d", w.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if body["status"] != "not_ready" {
		t.Errorf("expected status 'not_ready', got %q", body["status"])
	}
}

func TestHealthzReady(t *testing.T) {
	srv, err := platform.NewServer(platform.ServiceConfig{
		Name:    "test-service",
		Version: "0.0.1",
		Port:    9999,
	})
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	srv.SetReady()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	srv.Mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200 (ready), got %d", w.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("expected status 'ok', got %q", body["status"])
	}
	if body["service"] != "test-service" {
		t.Errorf("expected service 'test-service', got %q", body["service"])
	}
}

func TestMetricsEndpoint(t *testing.T) {
	srv, err := platform.NewServer(platform.ServiceConfig{
		Name:    "test-service",
		Version: "0.0.1",
		Port:    9999,
	})
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	srv.Mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200 for /metrics, got %d", w.Code)
	}

	// Prometheus metrics should contain at least the go_* metrics
	body := w.Body.String()
	if len(body) == 0 {
		t.Error("expected non-empty metrics response")
	}
}

func TestMustEnv(t *testing.T) {
	// Test default value
	result := platform.MustEnv("SN_TEST_NONEXISTENT_VAR", "default_val")
	if result != "default_val" {
		t.Errorf("expected 'default_val', got %q", result)
	}

	// Test with actual env var
	t.Setenv("SN_TEST_VAR", "custom_val")
	result = platform.MustEnv("SN_TEST_VAR", "default_val")
	if result != "custom_val" {
		t.Errorf("expected 'custom_val', got %q", result)
	}
}
