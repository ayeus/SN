package main

import (
	"encoding/json"
	"net/http"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func rawJSON(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(b)
}

// HandleRegions lists the regions customers can deploy to and hosts can join.
func (a *API) HandleRegions(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Pool.Query(r.Context(), `
		SELECT code, display_name, country_code, continent FROM regions WHERE active ORDER BY
		(country_code = 'IN') DESC, code;
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list regions")
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var code, name, country, continent string
		if err := rows.Scan(&code, &name, &country, &continent); err == nil {
			out = append(out, map[string]string{"code": code, "name": name, "country": country, "continent": continent})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"regions": out})
}

// HandleNetworkStats reports aggregate supply for the public site. It exposes
// counts only: no host ids, addresses, fingerprints or owners (the old public
// hosts endpoint leaked all of those).
func (a *API) HandleNetworkStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	window := int(a.heartbeatTimeout.Seconds())

	var hostsOnline, gpusOnline, vramOnline, deploymentsServing int
	var tokens24h, requests24h int64
	_ = a.db.Pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT h.id), COUNT(g.id), COALESCE(SUM(g.vram_gb), 0)
		FROM hosts h LEFT JOIN gpus g ON g.host_id = h.id
		WHERE h.deleted_at IS NULL AND h.status IN ('active', 'probation', 'draining')
		  AND h.last_heartbeat_at >= NOW() - ($1 * INTERVAL '1 second');
	`, window).Scan(&hostsOnline, &gpusOnline, &vramOnline)
	_ = a.db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM deployments WHERE state IN ('serving', 'degraded') AND deleted_at IS NULL;`).Scan(&deploymentsServing)
	_ = a.db.Pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(input_tokens + output_tokens), 0)
		FROM usage_events WHERE ts >= NOW() - INTERVAL '24 hours' AND status = 'success';
	`).Scan(&requests24h, &tokens24h)

	// Free GPUs are the ones a new deployment could be placed on right now
	// (the deploy wizard shows "N of M free" per tier).
	byTier := map[string]int{"t1": 0, "t2": 0, "t3": 0}
	freeByTier := map[string]int{"t1": 0, "t2": 0, "t3": 0}
	rows, err := a.db.Pool.Query(ctx, `
		SELECT h.tier, COUNT(g.id),
		       COUNT(g.id) FILTER (WHERE g.status = 'available' AND g.replica_id IS NULL AND NOT h.paused AND h.status <> 'draining')
		FROM hosts h JOIN gpus g ON g.host_id = h.id
		WHERE h.deleted_at IS NULL AND h.status IN ('active', 'probation', 'draining')
		  AND h.last_heartbeat_at >= NOW() - ($1 * INTERVAL '1 second')
		GROUP BY h.tier;
	`, window)
	if err == nil {
		for rows.Next() {
			var t string
			var n, free int
			if err := rows.Scan(&t, &n, &free); err == nil {
				byTier[t], freeByTier[t] = n, free
			}
		}
		rows.Close()
	}

	byRegion := []map[string]any{}
	rrows, err := a.db.Pool.Query(ctx, `
		SELECT h.region, COUNT(g.id) FROM hosts h JOIN gpus g ON g.host_id = h.id
		WHERE h.deleted_at IS NULL AND h.status IN ('active', 'probation', 'draining')
		  AND h.last_heartbeat_at >= NOW() - ($1 * INTERVAL '1 second')
		GROUP BY h.region ORDER BY 2 DESC;
	`, window)
	if err == nil {
		for rrows.Next() {
			var region string
			var n int
			if err := rrows.Scan(&region, &n); err == nil {
				byRegion = append(byRegion, map[string]any{"region": region, "gpus": n})
			}
		}
		rrows.Close()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"hosts_online":        hostsOnline,
		"gpus_online":         gpusOnline,
		"vram_gb_online":      vramOnline,
		"gpus_by_tier":        byTier,
		"gpus_free_by_tier":   freeByTier,
		"gpus_by_region":      byRegion,
		"deployments_serving": deploymentsServing,
		"requests_24h":        requests24h,
		"tokens_24h":          tokens24h,
	})
}
