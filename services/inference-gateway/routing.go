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
// tried again after the cooldown. Each failure in a row doubles the wait, up
// to maxCooldown, so a replica that fails every request is asked rarely
// instead of failing one request in every few seconds for ever.
const (
	unhealthyCooldown = 5 * time.Second
	maxCooldown       = 2 * time.Minute
)

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

	// maxInflight caps the requests one replica is given at a time. Zero
	// means no cap.
	maxInflight int

	mu          sync.Mutex
	outstanding map[string]int
	failedAt    map[string]time.Time
	failures    map[string]int // failures in a row
}

func newRouter(database *db.Client, heartbeatTimeout time.Duration) *router {
	return &router{
		db:               database,
		heartbeatTimeout: heartbeatTimeout,
		outstanding:      map[string]int{},
		failedAt:         map[string]time.Time{},
		failures:         map[string]int{},
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

// cooldown is how long a replica sits out after n failures in a row.
func cooldown(n int) time.Duration {
	d := unhealthyCooldown
	for ; n > 1 && d < maxCooldown; n-- {
		d *= 2
	}
	return min(d, maxCooldown)
}

// pick chooses the replica with the fewest outstanding requests, skipping
// excluded and recently failed replicas and those already answering as many
// requests as they are allowed. Ties are broken randomly so load spreads
// evenly across equally idle replicas.
//
// full reports that nothing was chosen only because every usable replica is
// at its limit: the deployment is busy, not broken.
func (r *router) pick(cands []replica, exclude map[string]bool) (rep replica, found, full bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pickLocked(cands, exclude)
}

func (r *router) pickLocked(cands []replica, exclude map[string]bool) (rep replica, found, full bool) {
	now := time.Now()
	best, bestLoad, ties := -1, 0, 0
	for i, c := range cands {
		if exclude[c.ID] {
			continue
		}
		if t, ok := r.failedAt[c.ID]; ok && now.Sub(t) < cooldown(r.failures[c.ID]) {
			continue
		}
		load := r.outstanding[c.ID]
		if r.maxInflight > 0 && load >= r.maxInflight {
			full = true
			continue
		}
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
		return replica{}, false, full
	}
	return cands[best], true, false
}

// take picks a replica and counts the request against it in one step, so two
// requests arriving together cannot both take a replica's last place.
func (r *router) take(cands []replica, exclude map[string]bool) (replica, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rep, found, full := r.pickLocked(cands, exclude)
	if found {
		r.outstanding[rep.ID]++
	}
	return rep, found, full
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
	r.failures[id]++
	r.mu.Unlock()
}

func (r *router) markHealthy(id string) {
	r.mu.Lock()
	delete(r.failedAt, id)
	delete(r.failures, id)
	r.mu.Unlock()
}
