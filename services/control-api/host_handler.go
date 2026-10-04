package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ayeus/ayeusann/internal/domain"
	"github.com/ayeus/ayeusann/internal/httpx"
	"github.com/ayeus/ayeusann/internal/lifecycle"
	"github.com/jackc/pgx/v5"
)

type IssueTokenRequest struct {
	// Tier the machine will enrol at (PRD F-10): t3 personal machines,
	// t2 labs and workstations, t1 data centres (platform approval required).
	Tier   string `json:"tier"`
	Region string `json:"region"`
	Label  string `json:"label"`
}

// HandleIssueRegistrationToken issues a single-use, 24-hour host enrolment
// token together with ready-to-paste install commands (PRD F-11).
func (a *API) HandleIssueRegistrationToken(w http.ResponseWriter, r *http.Request) {
	claims := claimsOf(r)
	var req IssueTokenRequest
	_ = httpxDecodeOptional(r, &req)

	req.Tier = strings.ToLower(strings.TrimSpace(req.Tier))
	if req.Tier == "" {
		req.Tier = domain.TierT3
	}
	switch req.Tier {
	case domain.TierT2, domain.TierT3:
	case domain.TierT1:
		// T1 onboarding is BD-led: audit, contract, VLAN (PRD F-10).
		if !a.isPlatformAdmin(r.Context(), claims.UserID) {
			writeError(w, http.StatusForbidden, "Tier 1 (data centre) hosts are onboarded with our team. Contact us, or enrol as Tier 2.")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "tier must be t1, t2 or t3")
		return
	}
	if req.Region == "" {
		req.Region = domain.RegionInSouth
	}
	if !domain.IsValidRegion(req.Region) {
		writeError(w, http.StatusBadRequest, "Unknown region")
		return
	}

	tok, err := a.tm.GenerateRegistrationToken(claims.UserID, claims.OrgID, req.Tier)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to issue registration token")
		return
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = "host-installer"
	}
	if _, err := a.db.Pool.Exec(r.Context(), `
		INSERT INTO host_registration_tokens (jti, user_id, org_id, label, expires_at)
		VALUES ($1, $2, $3, $4, $5);
	`, tok.JTI, claims.UserID, claims.OrgID, label, tok.ExpiresAt); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to record registration token")
		return
	}

	server, coordinator := a.publicBase(r), a.coordinatorBase(r)
	args := fmt.Sprintf("--server %s --token %s --coordinator %s --region %s", server, tok.Token, coordinator, req.Region)
	writeJSON(w, http.StatusCreated, map[string]any{
		"registration_token": tok.Token,
		"token_id":           tok.JTI,
		"expires_at":         tok.ExpiresAt,
		"tier":               req.Tier,
		"region":             req.Region,
		"server_url":         server,
		"coordinator_url":    coordinator,
		"commands": map[string]string{
			"linux_macos": fmt.Sprintf("curl -fsSL %s/install.sh | sh -s -- %s", server, args),
			"windows": fmt.Sprintf("& ([scriptblock]::Create((irm %s/install.ps1))) -Server %s -Token %s -Coordinator %s -Region %s",
				server, server, tok.Token, coordinator, req.Region),
			"windows_wsl": fmt.Sprintf("wsl -e sh -c \"curl -fsSL %s/install.sh | sh -s -- %s\"", server, args),
			"from_source": fmt.Sprintf("./run-node.sh --token %s --coordinator %s --region %s", tok.Token, coordinator, req.Region),
		},
	})
}

// HandleRegistrationTokenStatus reports whether an install token has been
// used and by which machine, so the console can show the new host the moment
// it enrols instead of guessing from timestamps.
func (a *API) HandleRegistrationTokenStatus(w http.ResponseWriter, r *http.Request) {
	var hostID *string
	var expires time.Time
	err := a.db.Pool.QueryRow(r.Context(), `
		SELECT consumed_by_host::TEXT, expires_at FROM host_registration_tokens
		WHERE jti::TEXT = $1 AND user_id = $2;
	`, r.PathValue("id"), claimsOf(r).UserID).Scan(&hostID, &expires)
	if err != nil {
		writeError(w, http.StatusNotFound, "Install command not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"used":    hostID != nil,
		"host_id": hostID,
		"expired": hostID == nil && time.Now().After(expires),
	})
}

// HostSummary is a host as its owner sees it.
type HostSummary struct {
	domain.Host
	Online        bool         `json:"online"`
	GPUs          []domain.GPU `json:"gpus"`
	ActiveJobs    int          `json:"active_jobs"`
	RequestsTotal int64        `json:"requests_total"`
	RequestsToday int64        `json:"requests_today"`
}

const hostColumns = `
	h.id, h.user_id, h.name, h.hostname, h.tier, h.region, h.overlay_ip, h.kyc_status, h.reputation, h.status,
	h.hw_fingerprint, h.agent_version, h.last_heartbeat_at, h.os, h.runtime, h.runtime_healthy, h.cached_models,
	h.paused, h.probation_until, h.created_at, h.updated_at`

func scanHost(row pgx.Row, extra ...any) (domain.Host, error) {
	var h domain.Host
	dest := append([]any{&h.ID, &h.UserID, &h.Name, &h.Hostname, &h.Tier, &h.Region, &h.OverlayIP, &h.KycStatus,
		&h.Reputation, &h.Status, &h.HwFingerprint, &h.AgentVersion, &h.LastHeartbeatAt, &h.OS, &h.Runtime,
		&h.RuntimeHealthy, &h.CachedModels, &h.Paused, &h.ProbationUntil, &h.CreatedAt, &h.UpdatedAt}, extra...)
	err := row.Scan(dest...)
	return h, err
}

func (a *API) online(h domain.Host) bool {
	return h.LastHeartbeatAt != nil && time.Since(*h.LastHeartbeatAt) < a.heartbeatTimeout &&
		h.Status != domain.HostStatusOffline && h.Status != domain.HostStatusBanned
}

func (a *API) gpusFor(ctx context.Context, hostIDs []string) (map[string][]domain.GPU, error) {
	out := map[string][]domain.GPU{}
	if len(hostIDs) == 0 {
		return out, nil
	}
	rows, err := a.db.Pool.Query(ctx, `
		SELECT id, host_id, model, vram_gb, driver_version, cuda_version, uuid, fingerprint, status, created_at
		FROM gpus WHERE host_id = ANY($1) ORDER BY model;
	`, hostIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var g domain.GPU
		if err := rows.Scan(&g.ID, &g.HostID, &g.Model, &g.VramGB, &g.DriverVersion, &g.CudaVersion, &g.UUID, &g.Fingerprint, &g.Status, &g.CreatedAt); err != nil {
			return nil, err
		}
		out[g.HostID] = append(out[g.HostID], g)
	}
	return out, rows.Err()
}

// HandleListHosts lists the caller's machines for the host dashboard (FR-81).
func (a *API) HandleListHosts(w http.ResponseWriter, r *http.Request) {
	claims := claimsOf(r)
	ctx := r.Context()
	rows, err := a.db.Pool.Query(ctx, `
		SELECT `+hostColumns+`,
		       (SELECT COUNT(*) FROM replicas x WHERE x.host_id = h.id AND x.state IN ('pending','pulling','loading','warming','serving','degraded')),
		       (SELECT COUNT(*) FROM usage_events u WHERE u.host_id = h.id AND u.status = 'success'),
		       (SELECT COUNT(*) FROM usage_events u WHERE u.host_id = h.id AND u.status = 'success' AND u.ts >= DATE_TRUNC('day', NOW()))
		FROM hosts h
		WHERE h.user_id = $1 AND h.deleted_at IS NULL
		ORDER BY h.created_at DESC;
	`, claims.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list hosts")
		return
	}
	defer rows.Close()

	list := []HostSummary{}
	var ids []string
	for rows.Next() {
		var s HostSummary
		h, err := scanHost(rows, &s.ActiveJobs, &s.RequestsTotal, &s.RequestsToday)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to read hosts")
			return
		}
		s.Host, s.Online = h, a.online(h)
		list = append(list, s)
		ids = append(ids, h.ID)
	}
	rows.Close()

	gpus, err := a.gpusFor(ctx, ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read GPUs")
		return
	}
	for i := range list {
		list[i].GPUs = gpus[list[i].ID]
		if list[i].GPUs == nil {
			list[i].GPUs = []domain.GPU{}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": list, "count": len(list)})
}

// ownHost loads a host the caller owns. Other users' hosts are 404, never 403.
func (a *API) ownHost(r *http.Request) (domain.Host, error) {
	return scanHost(a.db.Pool.QueryRow(r.Context(), `SELECT `+hostColumns+`
		FROM hosts h WHERE h.id::TEXT = $1 AND h.user_id = $2 AND h.deleted_at IS NULL;`,
		r.PathValue("id"), claimsOf(r).UserID))
}

// HandleGetHost is the host detail view: hardware, benchmark, reputation with
// its inputs ("why was I demoted", FR-63), and content-opaque jobs.
func (a *API) HandleGetHost(w http.ResponseWriter, r *http.Request) {
	h, err := a.ownHost(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "Host not found")
		return
	}
	ctx := r.Context()
	gpus, _ := a.gpusFor(ctx, []string{h.ID})

	type benchmark struct {
		ScoreCompute  *float64  `json:"score_compute"`
		VramBwGbps    *float64  `json:"vram_bw_gbps"`
		DiskReadMbps  *float64  `json:"disk_read_mbps"`
		DiskWriteMbps *float64  `json:"disk_write_mbps"`
		LatencyPopMs  *float64  `json:"latency_pop_ms"`
		RanAt         time.Time `json:"ran_at"`
	}
	var bm *benchmark
	var b benchmark
	if err := a.db.Pool.QueryRow(ctx, `
		SELECT score_compute, vram_bw_gbps, disk_read_mbps, disk_write_mbps, latency_pop_ms, ran_at
		FROM benchmarks WHERE host_id = $1 ORDER BY ran_at DESC LIMIT 1;
	`, h.ID).Scan(&b.ScoreCompute, &b.VramBwGbps, &b.DiskReadMbps, &b.DiskWriteMbps, &b.LatencyPopMs, &b.RanAt); err == nil {
		bm = &b
	}

	var rep *domain.ReputationSnapshot
	var s domain.ReputationSnapshot
	if err := a.db.Pool.QueryRow(ctx, `
		SELECT id, host_id, score, uptime_pct, correctness_pct, benchmark_stability, age_days, incident_rate, computed_at
		FROM reputation_snapshots WHERE host_id = $1 ORDER BY computed_at DESC LIMIT 1;
	`, h.ID).Scan(&s.ID, &s.HostID, &s.Score, &s.UptimePct, &s.CorrectnessPct, &s.BenchmarkStability, &s.AgeDays, &s.IncidentRate, &s.ComputedAt); err == nil {
		rep = &s
	}

	// Jobs are content-opaque (PRD F-14): the model and counts, never prompts.
	type job struct {
		ID        string     `json:"id"`
		ModelName string     `json:"model_name"`
		State     string     `json:"state"`
		Detail    *string    `json:"detail,omitempty"`
		StartedAt *time.Time `json:"started_at,omitempty"`
		Requests  int64      `json:"requests"`
		Failed    int64      `json:"failed"`
	}
	jobs := []job{}
	rows, err := a.db.Pool.Query(ctx, `
		SELECT r.id, m.name, r.state, r.detail, r.started_at, r.total_requests, r.failed_requests
		FROM replicas r JOIN deployments d ON d.id = r.deployment_id JOIN models m ON m.id = d.model_id
		WHERE r.host_id = $1 AND (r.state NOT IN ('stopped', 'failed') OR r.updated_at > NOW() - INTERVAL '1 day')
		ORDER BY r.created_at DESC LIMIT 50;
	`, h.ID)
	if err == nil {
		for rows.Next() {
			var j job
			if err := rows.Scan(&j.ID, &j.ModelName, &j.State, &j.Detail, &j.StartedAt, &j.Requests, &j.Failed); err == nil {
				jobs = append(jobs, j)
			}
		}
		rows.Close()
	}

	incidents := []map[string]any{}
	irows, err := a.db.Pool.Query(ctx, `
		SELECT kind, severity, action, resolved, created_at FROM trust_incidents
		WHERE host_id = $1 ORDER BY created_at DESC LIMIT 20;
	`, h.ID)
	if err == nil {
		for irows.Next() {
			var kind, sev string
			var action *string
			var resolved bool
			var at time.Time
			if err := irows.Scan(&kind, &sev, &action, &resolved, &at); err == nil {
				incidents = append(incidents, map[string]any{"kind": kind, "severity": sev, "action": action, "resolved": resolved, "created_at": at})
			}
		}
		irows.Close()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"host":       h,
		"online":     a.online(h),
		"gpus":       gpus[h.ID],
		"benchmark":  bm,
		"reputation": rep,
		"jobs":       jobs,
		"incidents":  incidents,
	})
}

// ActivitySummary is the work a host owner's machines have served.
type ActivitySummary struct {
	Today    WorkCount `json:"today"`
	MTD      WorkCount `json:"month_to_date"`
	Lifetime WorkCount `json:"lifetime"`
}

// WorkCount is successful requests and the tokens they processed.
type WorkCount struct {
	Requests int64 `json:"requests"`
	Tokens   int64 `json:"tokens"`
}

func (a *API) activity(ctx context.Context, hostFilter string, arg any) (ActivitySummary, []map[string]any, error) {
	var s ActivitySummary
	err := a.db.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE ts >= DATE_TRUNC('day', NOW())),
		       COALESCE(SUM(input_tokens + output_tokens) FILTER (WHERE ts >= DATE_TRUNC('day', NOW())), 0),
		       COUNT(*) FILTER (WHERE ts >= DATE_TRUNC('month', NOW())),
		       COALESCE(SUM(input_tokens + output_tokens) FILTER (WHERE ts >= DATE_TRUNC('month', NOW())), 0),
		       COUNT(*),
		       COALESCE(SUM(input_tokens + output_tokens), 0)
		FROM usage_events WHERE status = 'success' AND `+hostFilter+`;
	`, arg).Scan(&s.Today.Requests, &s.Today.Tokens, &s.MTD.Requests, &s.MTD.Tokens, &s.Lifetime.Requests, &s.Lifetime.Tokens)
	if err != nil {
		return s, nil, err
	}

	rows, err := a.db.Pool.Query(ctx, `
		SELECT DATE_TRUNC('day', ts) AS d, COUNT(*), COALESCE(SUM(input_tokens + output_tokens), 0)
		FROM usage_events WHERE status = 'success' AND `+hostFilter+` AND ts >= NOW() - INTERVAL '30 days'
		GROUP BY d ORDER BY d;
	`, arg)
	if err != nil {
		return s, nil, err
	}
	defer rows.Close()
	series := []map[string]any{}
	for rows.Next() {
		var d time.Time
		var n, tokens int64
		if err := rows.Scan(&d, &n, &tokens); err == nil {
			series = append(series, map[string]any{"date": d.Format("2006-01-02"), "requests": n, "tokens": tokens})
		}
	}
	return s, series, rows.Err()
}

// HandleHostActivity: today / month / lifetime, a 30-day series and a per-GPU
// breakdown for one host.
func (a *API) HandleHostActivity(w http.ResponseWriter, r *http.Request) {
	h, err := a.ownHost(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "Host not found")
		return
	}
	sum, series, err := a.activity(r.Context(), "host_id = $1", h.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read activity")
		return
	}
	perGPU := []map[string]any{}
	rows, err := a.db.Pool.Query(r.Context(), `
		SELECT g.model, g.uuid, COUNT(u.request_id), COALESCE(SUM(u.input_tokens + u.output_tokens), 0)
		FROM gpus g
		LEFT JOIN replicas rp ON rp.gpu_id = g.id
		LEFT JOIN usage_events u ON u.replica_id = rp.id AND u.status = 'success'
		WHERE g.host_id = $1 GROUP BY g.model, g.uuid ORDER BY g.model;
	`, h.ID)
	if err == nil {
		for rows.Next() {
			var model, uuid string
			var n, tokens int64
			if err := rows.Scan(&model, &uuid, &n, &tokens); err == nil {
				perGPU = append(perGPU, map[string]any{"gpu_model": model, "gpu_uuid": uuid, "requests": n, "tokens": tokens})
			}
		}
		rows.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{"summary": sum, "daily": series, "per_gpu": perGPU})
}

// HandleAllHostActivity aggregates every host the caller owns.
func (a *API) HandleAllHostActivity(w http.ResponseWriter, r *http.Request) {
	sum, series, err := a.activity(r.Context(),
		"host_id IN (SELECT id FROM hosts WHERE user_id = $1)", claimsOf(r).UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read activity")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"summary": sum, "daily": series})
}

// HandleHostTelemetry returns recent heartbeat samples for live charts.
func (a *API) HandleHostTelemetry(w http.ResponseWriter, r *http.Request) {
	h, err := a.ownHost(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "Host not found")
		return
	}
	minutes, _ := strconv.Atoi(r.URL.Query().Get("minutes"))
	if minutes <= 0 || minutes > 24*60 {
		minutes = 60
	}
	rows, err := a.db.Pool.Query(r.Context(), `
		SELECT ts, cpu_usage_pct, memory_usage_pct, gpu_status, active_jobs, host_user_active
		FROM host_telemetry WHERE host_id = $1 AND ts >= NOW() - ($2 * INTERVAL '1 minute')
		ORDER BY ts;
	`, h.ID, minutes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read telemetry")
		return
	}
	defer rows.Close()
	samples := []map[string]any{}
	for rows.Next() {
		var ts time.Time
		var cpu, mem float32
		var gpu []byte
		var jobs int
		var userActive bool
		if err := rows.Scan(&ts, &cpu, &mem, &gpu, &jobs, &userActive); err == nil {
			samples = append(samples, map[string]any{"ts": ts, "cpu_pct": cpu, "mem_pct": mem, "gpus": rawJSON(gpu), "active_jobs": jobs, "host_user_active": userActive})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"samples": samples})
}

type HostControlsRequest struct {
	Paused *bool   `json:"paused"`
	Name   *string `json:"name"`
}

// HandleHostControls pauses or resumes a host (UML §5 ACTIVE ⇄ DRAINING). A
// pause drains running jobs with the 30 s grace period and the scheduler moves
// the work elsewhere.
func (a *API) HandleHostControls(w http.ResponseWriter, r *http.Request) {
	h, err := a.ownHost(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "Host not found")
		return
	}
	var req HostControlsRequest
	if httpx.DecodeJSON(w, r, &req) != nil {
		return
	}
	ctx := r.Context()
	err = a.db.ExecTx(ctx, func(tx pgx.Tx) error {
		if req.Name != nil {
			name := strings.TrimSpace(*req.Name)
			if name == "" || len(name) > 100 {
				return errBadAction("Name must be 1–100 characters")
			}
			if _, err := tx.Exec(ctx, `UPDATE hosts SET name = $2, updated_at = NOW() WHERE id = $1;`, h.ID, name); err != nil {
				return err
			}
		}
		if req.Paused == nil {
			return nil
		}
		if h.Status == domain.HostStatusBanned {
			return errBadAction("This host is banned")
		}
		if *req.Paused {
			if _, err := tx.Exec(ctx, `
				UPDATE hosts SET paused = TRUE,
				       status = CASE WHEN status IN ('active', 'probation') THEN 'draining' ELSE status END,
				       updated_at = NOW()
				WHERE id = $1;
			`, h.ID); err != nil {
				return err
			}
			return drainHostReplicas(ctx, tx, h.ID, "host paused by its owner")
		}
		_, err := tx.Exec(ctx, `
			UPDATE hosts SET paused = FALSE,
			       status = CASE WHEN status = 'draining' THEN
			           CASE WHEN probation_until IS NOT NULL AND probation_until > NOW() THEN 'probation' ELSE 'active' END
			           ELSE status END,
			       updated_at = NOW()
			WHERE id = $1;
		`, h.ID)
		return err
	})
	var bad errBadAction
	if errors.As(err, &bad) {
		writeError(w, http.StatusConflict, string(bad))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update host")
		return
	}
	updated, _ := a.ownHost(r)
	writeJSON(w, http.StatusOK, map[string]any{"host": updated})
}

// drainHostReplicas marks a host's replicas STOPPING so the coordinator stops
// them gracefully while the scheduler places replacements.
func drainHostReplicas(ctx context.Context, tx pgx.Tx, hostID, reason string) error {
	rows, err := tx.Query(ctx, `
		SELECT id FROM replicas WHERE host_id = $1 AND state IN ('pending', 'pulling', 'loading', 'warming', 'serving', 'degraded');
	`, hostID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	deps := map[string]bool{}
	for _, id := range ids {
		depID, _, err := lifecycle.SetReplicaState(ctx, tx, id, lifecycle.Stopping, reason, "")
		if err != nil {
			return err
		}
		deps[depID] = true
	}
	for d := range deps {
		if _, _, err := lifecycle.Recompute(ctx, tx, d); err != nil {
			return err
		}
	}
	return nil
}

// HandleDecommissionHost removes a machine: its credential is revoked so the
// agent can no longer connect, and its jobs move elsewhere.
func (a *API) HandleDecommissionHost(w http.ResponseWriter, r *http.Request) {
	h, err := a.ownHost(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "Host not found")
		return
	}
	ctx := r.Context()
	err = a.db.ExecTx(ctx, func(tx pgx.Tx) error {
		if err := drainHostReplicas(ctx, tx, h.ID, "host decommissioned"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE hosts SET deleted_at = NOW(), credential_hash = NULL, paused = TRUE, hw_fingerprint = NULL, updated_at = NOW()
			WHERE id = $1;
		`, h.ID)
		return err
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to decommission host")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "decommissioned", "id": h.ID})
}
