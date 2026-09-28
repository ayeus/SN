// Package main implements the AyeusANN Request Router.
// Responsibilities: replica discovery, load-aware selection, health checks, retries, request proxying.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/domain"
)

var (
	ErrNoHealthyReplica = errors.New("router: no healthy serving replica found for deployment")
	ErrDeploymentNotFound = errors.New("router: deployment not found or not serving")
)

// RouteRequest is sent by the inference gateway to select a replica and proxy the request.
type RouteRequest struct {
	DeploymentID string          `json:"deployment_id"`
	Model        string          `json:"model"`
	OrgID        string          `json:"org_id"`
	Body         json.RawMessage `json:"body"` // Original ChatCompletion request body
	Stream       bool            `json:"stream"`
}

// RouteResponse wraps the proxied replica response metadata.
type RouteResponse struct {
	ReplicaID    string `json:"replica_id"`
	HostID       string `json:"host_id"`
	StatusCode   int    `json:"status_code"`
	Body         string `json:"body,omitempty"`
}

// replicaHealth tracks per-replica health state.
type replicaHealth struct {
	mu       sync.RWMutex
	healthy  map[string]bool    // replica_id → healthy
	reqCount map[string]int64   // replica_id → inflight request counter
}

func newReplicaHealth() *replicaHealth {
	return &replicaHealth{
		healthy:  make(map[string]bool),
		reqCount: make(map[string]int64),
	}
}

func (rh *replicaHealth) isHealthy(replicaID string) bool {
	rh.mu.RLock()
	defer rh.mu.RUnlock()
	h, exists := rh.healthy[replicaID]
	if !exists {
		return true // Assume healthy until proven otherwise
	}
	return h
}

func (rh *replicaHealth) markHealthy(replicaID string) {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	rh.healthy[replicaID] = true
}

func (rh *replicaHealth) markUnhealthy(replicaID string) {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	rh.healthy[replicaID] = false
}

func (rh *replicaHealth) incrementReqCount(replicaID string) {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	rh.reqCount[replicaID]++
}

func (rh *replicaHealth) decrementReqCount(replicaID string) {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	if rh.reqCount[replicaID] > 0 {
		rh.reqCount[replicaID]--
	}
}

func (rh *replicaHealth) getReqCount(replicaID string) int64 {
	rh.mu.RLock()
	defer rh.mu.RUnlock()
	return rh.reqCount[replicaID]
}

// RouterEngine manages replica discovery, selection, and request proxying.
type RouterEngine struct {
	db          *db.Client
	health      *replicaHealth
	httpClient  *http.Client
	maxRetries  int
}

// NewRouterEngine creates a new RouterEngine.
func NewRouterEngine(database *db.Client) *RouterEngine {
	return &RouterEngine{
		db:     database,
		health: newReplicaHealth(),
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		maxRetries: 2,
	}
}

// candidateReplica holds a serving replica's routing metadata.
type candidateReplica struct {
	ID            string
	DeploymentID  string
	HostID        string
	OverlayIP     string
	InferencePort int
	Tier          string // From host join
}

// discoverReplicas finds all serving replicas for a deployment, ordered by tier affinity.
func (e *RouterEngine) discoverReplicas(ctx context.Context, deploymentID string) ([]candidateReplica, error) {
	query := `
		SELECT r.id, r.deployment_id, r.host_id, 
		       COALESCE(HOST(r.overlay_ip), ''), COALESCE(r.inference_port, 0),
		       h.tier
		FROM replicas r
		JOIN hosts h ON h.id = r.host_id
		WHERE r.deployment_id = $1
		  AND r.state = 'serving'
		  AND r.healthy = TRUE
		  AND r.overlay_ip IS NOT NULL
		  AND r.inference_port > 0
		  AND h.status = 'active'
		  AND h.deleted_at IS NULL
		  AND h.last_heartbeat_at >= NOW() - INTERVAL '60 seconds'
		ORDER BY
		  CASE h.tier
		    WHEN 't1' THEN 1
		    WHEN 't2' THEN 2
		    WHEN 't3' THEN 3
		    ELSE 4
		  END,
		  h.reputation DESC;
	`

	rows, err := e.db.Pool.Query(ctx, query, deploymentID)
	if err != nil {
		return nil, fmt.Errorf("router: failed to query replicas: %w", err)
	}
	defer rows.Close()

	var replicas []candidateReplica
	for rows.Next() {
		var r candidateReplica
		if err := rows.Scan(&r.ID, &r.DeploymentID, &r.HostID, &r.OverlayIP, &r.InferencePort, &r.Tier); err != nil {
			return nil, fmt.Errorf("router: failed to scan replica: %w", err)
		}
		replicas = append(replicas, r)
	}

	return replicas, rows.Err()
}

// resolveDeploymentByModel resolves a model name to the active serving deployment.
func (e *RouterEngine) resolveDeploymentByModel(ctx context.Context, modelName, orgID string) (*domain.Deployment, error) {
	query := `
		SELECT d.id, d.org_id, d.model_id, d.name, d.state, d.tier, d.region,
		       d.min_replicas, d.max_replicas, d.burst_to_spot
		FROM deployments d
		JOIN models m ON m.id = d.model_id
		WHERE m.name = $1
		  AND d.org_id = $2
		  AND d.state = 'serving'
		  AND d.deleted_at IS NULL
		ORDER BY d.created_at DESC
		LIMIT 1;
	`

	var dep domain.Deployment
	err := e.db.Pool.QueryRow(ctx, query, modelName, orgID).Scan(
		&dep.ID, &dep.OrgID, &dep.ModelID, &dep.Name, &dep.State, &dep.Tier, &dep.Region,
		&dep.MinReplicas, &dep.MaxReplicas, &dep.BurstToSpot,
	)
	if err != nil {
		return nil, ErrDeploymentNotFound
	}
	return &dep, nil
}

// selectReplica picks the best healthy replica using least-connections strategy.
// Returns the selected replica and remaining candidates for retry.
func (e *RouterEngine) selectReplica(candidates []candidateReplica, excluded map[string]bool) (*candidateReplica, error) {
	var best *candidateReplica
	var bestLoad int64 = 1<<63 - 1

	// Shuffle within same tier to distribute load
	shuffled := make([]candidateReplica, len(candidates))
	copy(shuffled, candidates)
	rand.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})

	for i := range shuffled {
		r := &shuffled[i]
		if excluded[r.ID] {
			continue
		}
		if !e.health.isHealthy(r.ID) {
			continue
		}
		load := e.health.getReqCount(r.ID)
		if load < bestLoad {
			bestLoad = load
			best = r
		}
	}

	if best == nil {
		return nil, ErrNoHealthyReplica
	}
	return best, nil
}

// RouteAndProxy selects a replica and proxies the inference request.
// Implements retry logic: on 502/503 from a replica, retries on another replica up to maxRetries times.
func (e *RouterEngine) RouteAndProxy(ctx context.Context, req RouteRequest) (*RouteResponse, error) {
	// Resolve deployment from model name
	dep, err := e.resolveDeploymentByModel(ctx, req.Model, req.OrgID)
	if err != nil {
		return nil, err
	}

	// Discover serving replicas
	candidates, err := e.discoverReplicas(ctx, dep.ID)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, ErrNoHealthyReplica
	}

	excluded := make(map[string]bool)
	var lastErr error

	for attempt := 0; attempt <= e.maxRetries; attempt++ {
		replica, err := e.selectReplica(candidates, excluded)
		if err != nil {
			lastErr = err
			break
		}

		e.health.incrementReqCount(replica.ID)
		resp, proxyErr := e.proxyToReplica(ctx, replica, req.Body, req.Stream)
		e.health.decrementReqCount(replica.ID)

		if proxyErr != nil {
			// Mark failed replica for exclusion and retry
			excluded[replica.ID] = true
			e.health.markUnhealthy(replica.ID)
			lastErr = proxyErr
			continue
		}
		if err != nil {
			lastErr = err
			e.health.markUnhealthy(replica.ID)
			excluded[replica.ID] = true
			log.Printf("router: retryable error on replica %s: %v", replica.ID, err)
			continue
		}

		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("replica returned status %d", resp.StatusCode)
			e.health.markUnhealthy(replica.ID)
			excluded[replica.ID] = true
			log.Printf("router: replica %s returned status %d, retrying", replica.ID, resp.StatusCode)
			continue
		}

		return &RouteResponse{
			ReplicaID:  replica.ID,
			HostID:     replica.HostID,
			StatusCode: resp.StatusCode,
			Body:       resp.Body,
		}, nil
	}

	if lastErr != nil {
		return nil, fmt.Errorf("router: all retries exhausted: %w", lastErr)
	}
	return nil, ErrNoHealthyReplica
}

// proxyToReplica forwards the request body to a replica's inference endpoint.
func (e *RouterEngine) proxyToReplica(ctx context.Context, replica *candidateReplica, body json.RawMessage, stream bool) (*RouteResponse, error) {
	targetIP := replica.OverlayIP
	targetPort := replica.InferencePort

	if targetIP == "" || targetPort <= 0 {
		return nil, fmt.Errorf("router: replica %s has no overlay IP configured or invalid port (%s:%d)", replica.ID, targetIP, targetPort)
	}

	url := fmt.Sprintf("http://%s:%d/v1/chat/completions", targetIP, targetPort)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, io.NopCloser(jsonReader(body)))
	if err != nil {
		return nil, fmt.Errorf("router: failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("router: request to replica %s failed: %w", replica.ID, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("router: failed to read replica response: %w", err)
	}

	return &RouteResponse{
		ReplicaID:  replica.ID,
		HostID:     replica.HostID,
		StatusCode: resp.StatusCode,
		Body:       string(respBody),
	}, nil
}


// StartHealthChecker starts a background goroutine that periodically checks replica health.
func (e *RouterEngine) StartHealthChecker(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.checkAllReplicas(ctx)
			}
		}
	}()
}

// checkAllReplicas queries all serving replicas and pings their health endpoints.
func (e *RouterEngine) checkAllReplicas(ctx context.Context) {
	query := `
		SELECT r.id, COALESCE(HOST(r.overlay_ip), ''), COALESCE(r.inference_port, 0)
		FROM replicas r
		JOIN hosts h ON h.id = r.host_id
		WHERE r.state = 'serving'
		  AND h.status = 'active'
		  AND h.deleted_at IS NULL;
	`

	rows, err := e.db.Pool.Query(ctx, query)
	if err != nil {
		log.Printf("router: health check query failed: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id, overlayIP string
		var port int
		if err := rows.Scan(&id, &overlayIP, &port); err != nil {
			continue
		}

		if overlayIP == "" || port <= 0 {
			e.health.markUnhealthy(id)
			_, _ = e.db.Pool.Exec(ctx, "UPDATE replicas SET healthy = FALSE, last_health_check_at = NOW() WHERE id = $1", id)
			continue
		}

		go func(replicaID, ip string, p int) {
			url := fmt.Sprintf("http://%s:%d/healthz", ip, p)
			hCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			req, err := http.NewRequestWithContext(hCtx, http.MethodGet, url, nil)
			if err != nil {
				e.health.markUnhealthy(replicaID)
				_, _ = e.db.Pool.Exec(ctx, "UPDATE replicas SET healthy = FALSE, last_health_check_at = NOW() WHERE id = $1", replicaID)
				return
			}

			resp, err := e.httpClient.Do(req)
			if err != nil || resp.StatusCode != http.StatusOK {
				e.health.markUnhealthy(replicaID)
				_, _ = e.db.Pool.Exec(ctx, "UPDATE replicas SET healthy = FALSE, last_health_check_at = NOW() WHERE id = $1", replicaID)
				if resp != nil {
					_ = resp.Body.Close()
				}
				return
			}
			_ = resp.Body.Close()

			e.health.markHealthy(replicaID)
			_, _ = e.db.Pool.Exec(ctx, "UPDATE replicas SET healthy = TRUE, last_health_check_at = NOW() WHERE id = $1", replicaID)
		}(id, overlayIP, port)
	}
}

// jsonReader wraps json.RawMessage into an io.Reader.
func jsonReader(data json.RawMessage) io.Reader {
	return io.NopCloser(
		io.LimitReader(
			func() io.Reader {
				r, w := io.Pipe()
				go func() {
					_, _ = w.Write(data)
					w.Close()
				}()
				return r
			}(),
			int64(len(data)),
		),
	)
}
