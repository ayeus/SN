// Package platform provides shared server infrastructure for all SpazeNode services.
// Every service uses this package to get /healthz, /metrics, graceful shutdown,
// and structured logging out of the box.
package platform

import (
	"context"
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

// Server is the base server that every SpazeNode service embeds.
// It provides /healthz, /metrics, graceful shutdown, and structured logging.
type Server struct {
	Config  ServiceConfig
	Logger  *zap.Logger
	Mux     *http.ServeMux
	httpSrv *http.Server
	ready   atomic.Bool
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
func (s *Server) Run() error {
	s.httpSrv = &http.Server{
		Addr:              fmt.Sprintf(":%d", s.Config.Port),
		Handler:           s.Mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
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
		)
		if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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

// MustEnv reads an environment variable or returns a default value.
func MustEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
