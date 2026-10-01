// Package platform provides shared server infrastructure for all AyeusANN services.
// Every service uses this package to get /healthz, /metrics, graceful shutdown,
// and structured logging out of the box.
package platform

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// ServiceConfig holds the configuration for a platform service.
type ServiceConfig struct {
	Name    string
	Version string
	Port    int
}

// Server is the base server that every AyeusANN service embeds.
// It provides /healthz, /metrics, graceful shutdown, and structured logging.
type Server struct {
	Config     ServiceConfig
	Logger     *zap.Logger
	Mux        *http.ServeMux
	httpSrv    *http.Server
	ready      atomic.Bool
	readyCheck func(context.Context) error
}

// NewServer creates a new platform server with health and metrics endpoints.
func NewServer(cfg ServiceConfig) (*Server, error) {
	// Structured JSON logger
	zapCfg := zap.Config{
		Level:       zap.NewAtomicLevelAt(zap.InfoLevel),
		Development: os.Getenv("SN_ENV") == "dev",
		Encoding:    "json",
		EncoderConfig: zapcore.EncoderConfig{
			TimeKey:        "ts",
			LevelKey:       "level",
			NameKey:        "service",
			CallerKey:      "caller",
			MessageKey:     "msg",
			StacktraceKey:  "stacktrace",
			LineEnding:     zapcore.DefaultLineEnding,
			EncodeLevel:    zapcore.LowercaseLevelEncoder,
			EncodeTime:     zapcore.ISO8601TimeEncoder,
			EncodeDuration: zapcore.MillisDurationEncoder,
			EncodeCaller:   zapcore.ShortCallerEncoder,
		},
		OutputPaths:      []string{"stdout"},
		ErrorOutputPaths: []string{"stderr"},
	}

	logger, err := zapCfg.Build()
	if err != nil {
		return nil, fmt.Errorf("failed to create logger: %w", err)
	}
	logger = logger.Named(cfg.Name)

	mux := http.NewServeMux()
	s := &Server{
		Config: cfg,
		Logger: logger,
		Mux:    mux,
	}

	// Register standard endpoints
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.Handle("GET /metrics", promhttp.Handler())

	return s, nil
}

// SetReady marks the server as ready to receive traffic.
// Call this after all initialization is complete.
func (s *Server) SetReady() {
	s.ready.Store(true)
	s.Logger.Info("service ready", zap.String("service", s.Config.Name), zap.Int("port", s.Config.Port))
}

// Run starts the HTTP server and blocks until a shutdown signal is received.
// It performs graceful shutdown with a 15-second timeout.
//
// When TLS_CERT_FILE and TLS_KEY_FILE are set the server serves HTTPS with
// TLS 1.2 as the floor. Plain HTTP is intended for local development and for
// running behind a TLS-terminating ingress; production deployments should set
// either the cert pair or SN_TLS_TERMINATED_BY_PROXY=true to acknowledge that
// termination happens upstream.
func (s *Server) Run() error {
	certFile := os.Getenv("TLS_CERT_FILE")
	keyFile := os.Getenv("TLS_KEY_FILE")
	tlsEnabled := certFile != "" && keyFile != ""

	if !tlsEnabled && IsProduction() && !EnvBool("SN_TLS_TERMINATED_BY_PROXY", false) {
		return fmt.Errorf(
			"platform: refusing to serve plaintext HTTP in production; set TLS_CERT_FILE and TLS_KEY_FILE, " +
				"or set SN_TLS_TERMINATED_BY_PROXY=true if an ingress terminates TLS upstream")
	}

	s.httpSrv = &http.Server{
		Addr:              fmt.Sprintf(":%d", s.Config.Port),
		Handler:           s.accessLog(s.Mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // 0 = no deadline; SSE streams outlive any fixed write timeout
		IdleTimeout:       120 * time.Second,
	}

	if tlsEnabled {
		s.httpSrv.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			CurvePreferences: []tls.CurveID{
				tls.X25519, tls.CurveP256,
			},
		}
	}

	// Channel for shutdown signals
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// Channel for server errors
	errCh := make(chan error, 1)

	go func() {
		s.Logger.Info("starting server",
			zap.String("service", s.Config.Name),
			zap.String("version", s.Config.Version),
			zap.Int("port", s.Config.Port),
			zap.Bool("tls", tlsEnabled),
		)
		var err error
		if tlsEnabled {
			err = s.httpSrv.ListenAndServeTLS(certFile, keyFile)
		} else {
			err = s.httpSrv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("server error: %w", err)
	case sig := <-quit:
		s.Logger.Info("received shutdown signal", zap.String("signal", sig.String()))
	}

	return s.Shutdown()
}

// accessLog writes one structured line per request: method, path, status and
// duration. Probe and metrics endpoints are skipped; they would drown the log.
// Query strings are never logged, since they can carry tokens.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/readyz", "/metrics":
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		fields := []zap.Field{
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", status),
			zap.Duration("duration", time.Since(start)),
			zap.String("request_id", r.Header.Get("X-Request-ID")),
		}
		if status >= 500 {
			s.Logger.Error("request", fields...)
		} else {
			s.Logger.Info("request", fields...)
		}
	})
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	s.ready.Store(false)
	s.Logger.Info("shutting down gracefully")

	if err := s.httpSrv.Shutdown(ctx); err != nil {
		return fmt.Errorf("server shutdown error: %w", err)
	}

	_ = s.Logger.Sync()
	return nil
}

// handleHealthz returns the service health status.
// Returns 200 when ready, 503 when not ready.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	status := struct {
		Status  string `json:"status"`
		Service string `json:"service"`
		Version string `json:"version"`
	}{
		Service: s.Config.Name,
		Version: s.Config.Version,
	}

	if s.ready.Load() {
		status.Status = "ok"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	} else {
		status.Status = "not_ready"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	if err := json.NewEncoder(w).Encode(status); err != nil {
		log.Printf("failed to write healthz response: %v", err)
	}
}

// handleReadyz reports whether the service is ready to receive traffic.
// Kubernetes uses /healthz as a liveness probe and /readyz as a readiness probe;
// they differ once a service has dependencies that can degrade independently.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if !s.ready.Load() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"not_ready"}`))
		return
	}

	if s.readyCheck != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := s.readyCheck(ctx); err != nil {
			s.Logger.Warn("readiness check failed", zap.Error(err))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "not_ready",
				"reason": err.Error(),
			})
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// SetReadyCheck registers a dependency probe used by /readyz. Typically this
// pings the database so that a service with a dead pool is pulled from rotation.
func (s *Server) SetReadyCheck(fn func(context.Context) error) {
	s.readyCheck = fn
}
