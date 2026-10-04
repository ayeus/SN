// Package main implements the AyeusANN API Gateway: the single public entry
// point (Architecture §3). It routes the control-plane API, the OpenAI-compatible
// inference API, installer downloads and the web console.
package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/httpx"
	"github.com/ayeus/ayeusann/internal/platform"
	"github.com/google/uuid"
)

// Upstreams the gateway forwards to.
type Upstreams struct {
	ControlAPI, Inference, Billing, Trust, Web *url.URL
	// InstallDir holds install.sh / install.ps1; DownloadsDir holds prebuilt
	// agent binaries named ayeusann-agent-<os>-<arch>.
	InstallDir, DownloadsDir string
	// InferenceHost, when set, routes every request on that host (and its
	// {deployment}.subdomains) to the inference plane (SRS FR-22).
	InferenceHost string
	CORSOrigins   []string
	// TrustProxy keeps the X-Forwarded-For chain of a TLS-terminating proxy in
	// front of the gateway, so services see the real client address. Leave it
	// off when the gateway is the edge: a client could forge the header.
	TrustProxy bool
}

func newProxy(target *url.URL, name string, trustProxy bool) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			if trustProxy {
				// SetXForwarded appends to the outbound chain, which Rewrite starts empty.
				pr.Out.Header["X-Forwarded-For"] = pr.In.Header["X-Forwarded-For"]
			}
			pr.SetXForwarded()
			if proto := pr.In.Header.Get("X-Forwarded-Proto"); trustProxy && proto != "" {
				pr.Out.Header.Set("X-Forwarded-Proto", proto)
			}
			pr.Out.Host = pr.In.Host // services build absolute URLs from it
		},
		// Flush every write: SSE tokens must reach the client as they arrive.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			httpx.WriteProblem(w, http.StatusBadGateway, fmt.Sprintf("%s is unavailable", name))
		},
	}
}

// isInferenceRequest decides whether GET /v1/models belongs to the OpenAI
// surface. The docs put the control plane and the inference plane on separate
// hostnames; on a single host, an API-key caller (an OpenAI SDK) gets the
// OpenAI list and a console session gets the catalogue.
func isInferenceRequest(r *http.Request) bool {
	if r.Header.Get("X-API-Key") != "" {
		return true
	}
	h := r.Header.Get("Authorization")
	return len(h) > 7 && strings.HasPrefix(strings.TrimSpace(h[7:]), auth.APIKeyPrefix)
}

// NewHandler builds the gateway's router.
func NewHandler(u Upstreams) http.Handler {
	control := newProxy(u.ControlAPI, "control-api", u.TrustProxy)
	inference := newProxy(u.Inference, "inference-gateway", u.TrustProxy)
	billingP := newProxy(u.Billing, "billing-meter", u.TrustProxy)
	trust := newProxy(u.Trust, "trust-engine", u.TrustProxy)
	var web http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("The web console is not running. Start it with `make web` (or set WEB_URL).\n"))
	})
	if u.Web != nil {
		web = newProxy(u.Web, "web console", u.TrustProxy)
	}

	mux := http.NewServeMux()

	// Inference plane (OpenAI wire format).
	for _, p := range []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"} {
		mux.Handle(p, inference)
	}
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if isInferenceRequest(r) {
			inference.ServeHTTP(w, r)
			return
		}
		control.ServeHTTP(w, r)
	})

	// Billing.
	for _, p := range []string{"/v1/billing/", "/v1/usage", "/v1/invoices", "/v1/invoices/"} {
		mux.Handle(p, billingP)
	}
	// Trust (owner-scoped reads only; writes are internal).
	mux.Handle("GET /v1/reputation/", trust)
	// Everything else under /v1 is the control API.
	mux.Handle("/v1/", control)

	// Service internals are never reachable from outside.
	mux.HandleFunc("/internal/", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteProblem(w, http.StatusNotFound, "Not found")
	})

	// Installers and agent binaries (PRD F-11: one-command install).
	serveFile := func(dir, name, ctype string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ctype)
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFile(w, r, filepath.Join(dir, name))
		}
	}
	mux.HandleFunc("GET /install.sh", serveFile(u.InstallDir, "install.sh", "text/x-shellscript; charset=utf-8"))
	mux.HandleFunc("GET /install.ps1", serveFile(u.InstallDir, "install.ps1", "text/plain; charset=utf-8"))
	mux.HandleFunc("GET /downloads/{file}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("file")
		if !strings.HasPrefix(name, "ayeusann-agent-") || strings.ContainsAny(name, `/\`) {
			http.NotFound(w, r)
			return
		}
		path := filepath.Join(u.DownloadsDir, name)
		if _, err := os.Stat(path); err != nil {
			httpx.WriteProblem(w, http.StatusNotFound, "No prebuilt agent for this platform; the installer will build from source")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, path)
	})

	// The web console owns every other path.
	mux.Handle("/", web)

	var h http.Handler = mux
	if u.InferenceHost != "" {
		inner := h
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := strings.Split(r.Host, ":")[0]
			if (host == u.InferenceHost || strings.HasSuffix(host, "."+u.InferenceHost)) && strings.HasPrefix(r.URL.Path, "/v1/") {
				inference.ServeHTTP(w, r)
				return
			}
			inner.ServeHTTP(w, r)
		})
	}

	return withRequestID(httpx.CORS(u.CORSOrigins, h))
}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 128 {
			id = uuid.NewString()
			r.Header.Set("X-Request-ID", id)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}

func mustURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		log.Fatalf("configuration error: invalid upstream URL %q", raw)
	}
	return u
}

func main() {
	port := platform.EnvInt("GATEWAY_PORT", 8080)
	srv, err := platform.NewServer(platform.ServiceConfig{Name: "gateway", Version: "0.3.0", Port: port})
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	var web *url.URL
	if raw := platform.WebURL(); raw != "" {
		web = mustURL(raw)
	}
	handler := NewHandler(Upstreams{
		ControlAPI:    mustURL(platform.ControlAPIURL()),
		Inference:     mustURL(platform.InferenceGatewayURL()),
		Billing:       mustURL(platform.BillingMeterURL()),
		Trust:         mustURL(platform.TrustEngineURL()),
		Web:           web,
		InstallDir:    platform.Env("INSTALL_DIR", "web/install"),
		DownloadsDir:  platform.Env("DOWNLOADS_DIR", "dist/agent"),
		InferenceHost: platform.Env("INFERENCE_HOST", ""),
		CORSOrigins:   httpx.SplitList(platform.Env("CORS_ALLOWED_ORIGINS", "")),
		TrustProxy:    platform.EnvBool("TRUST_PROXY_HEADERS", false),
	})

	srv.Mux.Handle("/", platform.MetricsMiddleware("gateway", handler))
	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
