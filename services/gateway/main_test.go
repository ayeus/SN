package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// upstream records which service received a request.
func upstream(name string) (*httptest.Server, *url.URL) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-By", name)
		w.Header().Set("X-Seen-Host", r.Host)
		_, _ = io.WriteString(w, name)
	}))
	u, _ := url.Parse(s.URL)
	return s, u
}

func setup(t *testing.T) http.Handler {
	t.Helper()
	var ups []*httptest.Server
	mk := func(name string) *url.URL {
		s, u := upstream(name)
		ups = append(ups, s)
		return u
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "install.sh"), []byte("#!/bin/sh\necho install\n"), 0o644)
	h := NewHandler(Upstreams{
		ControlAPI: mk("control"), Inference: mk("inference"), Billing: mk("billing"),
		Trust: mk("trust"), Web: mk("web"), InstallDir: dir, DownloadsDir: dir,
		CORSOrigins: []string{"http://localhost:3000"},
	})
	t.Cleanup(func() {
		for _, s := range ups {
			s.Close()
		}
	})
	return h
}

func route(h http.Handler, method, path string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRouting(t *testing.T) {
	h := setup(t)
	cases := []struct {
		method, path string
		headers      []string
		want         string
	}{
		{"POST", "/v1/chat/completions", nil, "inference"},
		{"POST", "/v1/completions", nil, "inference"},
		{"POST", "/v1/embeddings", nil, "inference"},
		{"GET", "/v1/models", []string{"Authorization", "Bearer sk_live_abcd1234"}, "inference"},
		{"GET", "/v1/models", []string{"X-API-Key", "sk_live_abcd1234"}, "inference"},
		{"GET", "/v1/models", []string{"Authorization", "Bearer eyJhbGciOi"}, "control"},
		{"GET", "/v1/models", nil, "control"},
		{"GET", "/v1/billing/wallet", nil, "billing"},
		{"POST", "/v1/billing/topup", nil, "billing"},
		{"GET", "/v1/usage", nil, "billing"},
		{"GET", "/v1/invoices", nil, "billing"},
		{"GET", "/v1/invoices/abc", nil, "billing"},
		{"GET", "/v1/reputation/abc", nil, "trust"},
		{"POST", "/v1/deployments", nil, "control"},
		{"GET", "/v1/hosts/abc/earnings", nil, "control"},
		{"GET", "/", nil, "web"},
		{"GET", "/app/deployments", nil, "web"},
	}
	for _, c := range cases {
		rec := route(h, c.method, c.path, c.headers...)
		if got := rec.Header().Get("X-Served-By"); got != c.want {
			t.Errorf("%s %s → %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

func TestInternalRoutesAreNeverExposed(t *testing.T) {
	h := setup(t)
	for _, p := range []string{"/internal/v1/infer", "/internal/v1/incidents"} {
		if rec := route(h, "POST", p); rec.Code != http.StatusNotFound || rec.Header().Get("X-Served-By") != "" {
			t.Errorf("%s reached an upstream (%d)", p, rec.Code)
		}
	}
	// The old public scheduler endpoint must not bypass auth either: it now
	// goes to the control API, which has no such route.
	if rec := route(h, "POST", "/v1/schedule"); rec.Header().Get("X-Served-By") != "control" {
		t.Errorf("/v1/schedule should fall through to control-api")
	}
}

func TestCORSIsAnAllowlist(t *testing.T) {
	h := setup(t)
	ok := route(h, "GET", "/v1/models", "Origin", "http://localhost:3000")
	if ok.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Fatal("allowed origin not echoed")
	}
	bad := route(h, "GET", "/v1/models", "Origin", "https://evil.example")
	if v := bad.Header().Get("Access-Control-Allow-Origin"); v != "" {
		t.Fatalf("disallowed origin got CORS header %q", v)
	}
	pre := route(h, "OPTIONS", "/v1/deployments", "Origin", "http://localhost:3000", "Access-Control-Request-Method", "POST")
	if pre.Code != http.StatusNoContent {
		t.Fatalf("preflight status %d", pre.Code)
	}
}

func TestInstallerAndDownloads(t *testing.T) {
	h := setup(t)
	rec := route(h, "GET", "/install.sh")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "echo install") {
		t.Fatalf("install.sh: %d %q", rec.Code, rec.Body.String())
	}
	if rec := route(h, "GET", "/downloads/../../etc/passwd"); rec.Code == http.StatusOK {
		t.Fatal("path traversal in downloads")
	}
	if rec := route(h, "GET", "/downloads/ayeusann-agent-linux-amd64"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing binary should 404, got %d", rec.Code)
	}
}

func TestRequestIDAndHostPreserved(t *testing.T) {
	h := setup(t)
	req := httptest.NewRequest("GET", "/v1/deployments", nil)
	req.Host = "console.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing request id")
	}
	if rec.Header().Get("X-Seen-Host") != "console.example.com" {
		t.Fatalf("upstream saw host %q", rec.Header().Get("X-Seen-Host"))
	}
}
