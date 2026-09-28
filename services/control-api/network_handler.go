package main

import (
	"encoding/json"
	"net/http"

	"github.com/ayeus/ayeusann/internal/billing"
	"github.com/ayeus/ayeusann/internal/money"
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

// HandlePricing publishes everything the UI needs to show prices, so no price
// or policy is duplicated in frontend code: the GPU rate card with live
// availability (PRD §9, F-3), FX rates, the host revenue share and the spot
// discount.
func (a *API) HandlePricing(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := a.db.Pool.Query(ctx, `
		SELECT s.id, s.gpu_model, s.vram_gb, s.tdp_watts, s.tier, s.price_per_hour_inr, s.price_per_hour_usd, s.is_spot,
		       COUNT(g.id) FILTER (WHERE g.id IS NOT NULL),
		       COUNT(g.id) FILTER (WHERE g.status = 'available' AND g.replica_id IS NULL)
		FROM gpu_skus s
		LEFT JOIN hosts h ON h.tier = s.tier AND h.deleted_at IS NULL AND h.status IN ('active', 'probation')
		     AND NOT h.paused AND h.last_heartbeat_at >= NOW() - ($1 * INTERVAL '1 second')
		LEFT JOIN gpus g ON g.host_id = h.id AND g.model ILIKE s.match_pattern
		WHERE s.active
		GROUP BY s.id ORDER BY s.display_order;
	`, int(a.heartbeatTimeout.Seconds()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read rate card")
		return
	}
	defer rows.Close()
	skus := []map[string]any{}
	for rows.Next() {
		var id, model, tier string
		var vram, tdp int
		var inr, usd money.Amount
		var spot bool
		var online, free int
		if err := rows.Scan(&id, &model, &vram, &tdp, &tier, &inr, &usd, &spot, &online, &free); err != nil {
			continue
		}
		availability := 0.0
		if online > 0 {
			availability = float64(free) / float64(online)
		}
		skus = append(skus, map[string]any{
			"id": id, "gpu_model": model, "vram_gb": vram, "tdp_watts": tdp, "tier": tier, "is_spot": spot,
			"price_per_hour_inr": money.FromMicros(inr.Micros(), "INR"),
			"price_per_hour_usd": money.FromMicros(usd.Micros(), "USD"),
			"online_gpus":        online, "free_gpus": free, "availability": availability,
		})
	}
	rows.Close()

	fx := map[string]string{}
	frows, err := a.db.Pool.Query(ctx, `SELECT quote, rate::TEXT, updated_at FROM fx_rates WHERE base = 'USD';`)
	if err == nil {
		for frows.Next() {
			var q, rate string
			var at any
			if err := frows.Scan(&q, &rate, &at); err == nil {
				fx[q] = rate
			}
		}
		frows.Close()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"price_currency":     billing.PriceCurrency,
		"fx_from_usd":        fx,
		"host_share_percent": billing.HostSharePercent,
		"spot_price_percent": billing.SpotPricePercent,
		"gpu_skus":           skus,
	})
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

	byTier := map[string]int{"t1": 0, "t2": 0, "t3": 0}
	rows, err := a.db.Pool.Query(ctx, `
		SELECT h.tier, COUNT(g.id) FROM hosts h JOIN gpus g ON g.host_id = h.id
		WHERE h.deleted_at IS NULL AND h.status IN ('active', 'probation', 'draining')
		  AND h.last_heartbeat_at >= NOW() - ($1 * INTERVAL '1 second')
		GROUP BY h.tier;
	`, window)
	if err == nil {
		for rows.Next() {
			var t string
			var n int
			if err := rows.Scan(&t, &n); err == nil {
				byTier[t] = n
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
		"gpus_by_region":      byRegion,
		"deployments_serving": deploymentsServing,
		"requests_24h":        requests24h,
		"tokens_24h":          tokens24h,
	})
}
