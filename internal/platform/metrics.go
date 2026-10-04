package platform

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Application metrics exposed on /metrics by every service.
// Previously /metrics served only the default Go collectors, so there was no
// way to see request rates, latency, inference outcomes, or usage-record failures.
var (
	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ayeusann_http_requests_total",
		Help: "Total HTTP requests by service, route, method, and status class.",
	}, []string{"service", "route", "method", "status"})

	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ayeusann_http_request_duration_seconds",
		Help:    "HTTP request latency in seconds.",
		Buckets: []float64{0.005, 0.025, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
	}, []string{"service", "route", "method"})

	InferenceRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ayeusann_inference_requests_total",
		Help: "Inference requests by model, tier, and outcome (success|error|rejected).",
	}, []string{"model", "tier", "outcome"})

	InferenceTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ayeusann_inference_tokens_total",
		Help: "Tokens processed by model and direction (input|output).",
	}, []string{"model", "direction"})

	InferenceLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ayeusann_inference_duration_seconds",
		Help:    "End-to-end inference latency by model and tier.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120},
	}, []string{"model", "tier"})

	RouterReplicaSelections = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ayeusann_router_replica_selections_total",
		Help: "Replica selections by tier and result (selected|no_capacity|unhealthy).",
	}, []string{"tier", "result"})

	RouterHealthyReplicas = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ayeusann_router_healthy_replicas",
		Help: "Replicas currently passing health checks, by deployment.",
	}, []string{"deployment_id"})

	UsageRecordFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ayeusann_usage_record_failures_total",
		Help: "Served requests whose usage event could not be written. Alert on any increase.",
	})

	HostsConnected = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ayeusann_hosts_connected",
		Help: "Host agents with a live coordinator session.",
	})

	AuthFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ayeusann_auth_failures_total",
		Help: "Authentication failures by service and reason.",
	}, []string{"service", "reason"})
)

// uuidPattern matches a canonical UUID path segment.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// normalizeRoute collapses identifier path segments into placeholders so that
// per-org and per-host paths do not create unbounded Prometheus label cardinality.
func normalizeRoute(path string) string {
	if path == "" {
		return "unmatched"
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		switch {
		case seg == "":
		case uuidPattern.MatchString(seg):
			segments[i] = ":id"
		case len(seg) > 24:
			// Opaque tokens and hashes; keep the label bounded.
			segments[i] = ":opaque"
		}
	}
	return strings.Join(segments, "/")
}

// statusRecorder captures the response status code for metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Flush forwards to the underlying ResponseWriter so SSE streaming keeps working
// when the handler is wrapped by MetricsMiddleware.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards connection takeover. Without it httputil.ReverseProxy cannot
// upgrade to WebSocket ("can't switch protocols using non-Hijacker
// ResponseWriter"), which broke the web console behind the gateway: the Next.js
// dev client waits for its HMR WebSocket before it hydrates, so every button on
// every page did nothing.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("platform: underlying ResponseWriter does not support hijacking")
	}
	if r.status == 0 {
		r.status = http.StatusSwitchingProtocols
	}
	return h.Hijack()
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// MetricsMiddleware records request counts and latency for every request.
// The route label comes from the matched ServeMux pattern rather than the raw
// path, so high-cardinality path parameters do not explode the metric series.
func MetricsMiddleware(service string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(rec, r)

		route := normalizeRoute(r.URL.Path)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}

		HTTPRequestsTotal.WithLabelValues(service, route, r.Method, strconv.Itoa(rec.status)).Inc()
		HTTPRequestDuration.WithLabelValues(service, route, r.Method).Observe(time.Since(start).Seconds())
	})
}
