package main

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/ayeus/ayeusann/internal/db"
)

// This file is the request router of Architecture §4 ("replica selection,
// health, retries, tier policy"), embedded in the inference gateway (ADR-011):
// both run in the PoP, and a separate hop only added latency and buffering.

// unhealthyCooldown keeps a replica that just failed out of rotation. NFR-2
// requires failover in under 5 s; a failed replica is skipped immediately and
// retried after the cooldown.
const unhealthyCooldown = 5 * time.Second

// replica is a serving replica eligible for traffic.
type replica struct {
	ID       string
	HostID   string
	HostTier string
	GPUModel string
}

// router tracks outstanding requests and recent failures per replica.
type router struct {
	db               *db.Client
	heartbeatTimeout time.Duration

	mu          sync.Mutex
	outstanding map[string]int
	failedAt    map[string]time.Time
}

func newRouter(database *db.Client, heartbeatTimeout time.Duration) *router {
	return &router{
		db:               database,
		heartbeatTimeout: heartbeatTimeout,
		outstanding:      map[string]int{},
		failedAt:         map[string]time.Time{},
	}
}

// candidates lists serving replicas of a deployment on live hosts.
func (r *router) candidates(ctx context.Context, deploymentID string) ([]replica, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT r.id, r.host_id, h.tier, COALESCE(g.model, '')
		FROM replicas r
		JOIN hosts h ON h.id = r.host_id
		LEFT JOIN gpus g ON g.id = r.gpu_id
		WHERE r.deployment_id = $1
		  AND r.state = 'serving'
		  AND h.deleted_at IS NULL
		  AND h.status IN ('active', 'probation')
		  AND h.last_heartbeat_at >= NOW() - ($2 * INTERVAL '1 second');
	`, deploymentID, int(r.heartbeatTimeout.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []replica
	for rows.Next() {
		var x replica
		if err := rows.Scan(&x.ID, &x.HostID, &x.HostTier, &x.GPUModel); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// pick chooses the replica with the fewest outstanding requests, skipping
// excluded and recently failed replicas. Ties are broken randomly so load
// spreads evenly across equally idle replicas.
func (r *router) pick(cands []replica, exclude map[string]bool) (replica, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	best, bestLoad, ties := -1, 0, 0
	for i, c := range cands {
		if exclude[c.ID] {
			continue
		}
		if t, ok := r.failedAt[c.ID]; ok && now.Sub(t) < unhealthyCooldown {
			continue
		}
		load := r.outstanding[c.ID]
		switch {
		case best == -1 || load < bestLoad:
			best, bestLoad, ties = i, load, 1
		case load == bestLoad:
			ties++
			if rand.IntN(ties) == 0 {
				best = i
			}
		}
	}
	if best == -1 {
		return replica{}, false
	}
	return cands[best], true
}

func (r *router) acquire(id string) {
	r.mu.Lock()
	r.outstanding[id]++
	r.mu.Unlock()
}

func (r *router) release(id string) {
	r.mu.Lock()
	if r.outstanding[id] > 1 {
		r.outstanding[id]--
	} else {
		delete(r.outstanding, id)
	}
	r.mu.Unlock()
}

func (r *router) markFailed(id string) {
	r.mu.Lock()
	r.failedAt[id] = time.Now()
	r.mu.Unlock()
}

func (r *router) markHealthy(id string) {
	r.mu.Lock()
	delete(r.failedAt, id)
	r.mu.Unlock()
}
