package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ayeus/ayeusann/internal/domain"
	"github.com/ayeus/ayeusann/internal/httpx"
	"github.com/ayeus/ayeusann/internal/lifecycle"
	"github.com/ayeus/ayeusann/internal/placement"
	"github.com/jackc/pgx/v5"
)

// Cluster size bounds from PRD F-3 ("cluster size 1–16").
const maxReplicas = 16

// deploymentNamePattern: names are how customers address deployments in the
// OpenAI "model" field, so they are slugs.
var deploymentNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$`)

type CreateDeploymentRequest struct {
	ModelID      string `json:"model_id"`
	Name         string `json:"name"`
	Tier         string `json:"tier"`
	Region       string `json:"region"`
	MinReplicas  int    `json:"min_replicas"`
	MaxReplicas  int    `json:"max_replicas"`
	ScaleToZero  bool   `json:"scale_to_zero"`
	BurstToSpot  bool   `json:"burst_to_spot"`
	ResidentIN   bool   `json:"resident_in"`
	Quantization string `json:"quantization"`
}

// DeploymentView is a deployment as the console shows it.
type DeploymentView struct {
	domain.Deployment
	ModelName       string        `json:"model_name"`
	ReplicasServing int           `json:"replicas_serving"`
	ReplicasActive  int           `json:"replicas_active"`
	Usage24h        *UsageSummary `json:"usage_24h,omitempty"`
}

// UsageSummary aggregates usage events.
type UsageSummary struct {
	Requests     int64 `json:"requests"`
	Errors       int64 `json:"errors"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

const deploymentColumns = `
	d.id, d.org_id, d.model_id, d.name, d.state, d.tier, d.region, d.min_replicas, d.max_replicas,
	d.scale_to_zero, d.burst_to_spot, d.resident_in, d.endpoint, d.quantization, d.config_json,
	d.desired_state, d.last_error, d.state_changed_at, d.created_at, d.updated_at, m.name`

func scanDeployment(row pgx.Row, extra ...any) (DeploymentView, error) {
	var v DeploymentView
	d := &v.Deployment
	dest := append([]any{&d.ID, &d.OrgID, &d.ModelID, &d.Name, &d.State, &d.Tier, &d.Region, &d.MinReplicas, &d.MaxReplicas,
		&d.ScaleToZero, &d.BurstToSpot, &d.ResidentIn, &d.Endpoint, &d.Quantization, &d.ConfigJSON,
		&d.DesiredState, &d.LastError, &d.StateChangedAt, &d.CreatedAt, &d.UpdatedAt, &v.ModelName}, extra...)
	err := row.Scan(dest...)
	return v, err
}

// HandleCreateDeployment implements POST /v1/deployments (SRS FR-20). The
// deployment is created PENDING; the scheduler places it and the coordinator
// dispatches it, so this call returns immediately (Architecture §8).
func (a *API) HandleCreateDeployment(w http.ResponseWriter, r *http.Request) {
	claims := claimsOf(r)
	var req CreateDeploymentRequest
	if httpx.DecodeJSON(w, r, &req) != nil {
		return
	}
	ctx := r.Context()

	var model struct {
		name    string
		tiers   []string
		isBYO   bool
		presets []byte
	}
	err := a.db.Pool.QueryRow(ctx, `
		SELECT name, tiers_allowed, is_byo, quantization_presets FROM models
		WHERE id::TEXT = $1 AND (owner_org_id IS NULL OR owner_org_id = $2);
	`, req.ModelID, claims.OrgID).Scan(&model.name, &model.tiers, &model.isBYO, &model.presets)
	if err != nil {
		httpx.WriteProblemFields(w, http.StatusNotFound, "Model not found", map[string]string{"model_id": "Choose a model from the catalogue"})
		return
	}

	req.Name = strings.ToLower(strings.TrimSpace(req.Name))
	if req.Name == "" {
		req.Name = strings.ReplaceAll(model.name, ".", "-")
	}
	req.Tier = strings.ToLower(strings.TrimSpace(req.Tier))
	if req.Tier == "" {
		req.Tier = domain.TierT3
	}
	if req.Region == "" {
		_ = a.db.Pool.QueryRow(ctx, `SELECT default_region FROM organizations WHERE id = $1;`, claims.OrgID).Scan(&req.Region)
	}
	if req.MinReplicas <= 0 {
		req.MinReplicas = 1
	}
	if req.MaxReplicas < req.MinReplicas {
		req.MaxReplicas = req.MinReplicas
	}

	fields := map[string]string{}
	if !deploymentNamePattern.MatchString(req.Name) {
		fields["name"] = "3–64 lowercase letters, digits or dashes"
	}
	switch {
	case req.Tier != domain.TierT1 && req.Tier != domain.TierT2 && req.Tier != domain.TierT3:
		fields["tier"] = "Choose T1, T2 or T3"
	case !slices.Contains(model.tiers, req.Tier):
		fields["tier"] = fmt.Sprintf("%s is available on %s only", model.name, strings.ToUpper(strings.Join(model.tiers, ", ")))
	case model.isBYO && req.Tier == domain.TierT3:
		fields["tier"] = "Bring-your-own models never run on Tier 3 personal machines"
	}
	if !domain.IsValidRegion(req.Region) {
		fields["region"] = "Unknown region"
	} else if req.ResidentIN && !strings.HasPrefix(req.Region, "IN-") {
		fields["region"] = "India-resident deployments must use an Indian region"
	}
	if req.MinReplicas > maxReplicas || req.MaxReplicas > maxReplicas {
		fields["min_replicas"] = fmt.Sprintf("At most %d replicas", maxReplicas)
	}
	if req.Quantization != "" && !presetExists(model.presets, req.Quantization) {
		fields["quantization"] = "Unknown quantization preset for this model"
	}
	if len(fields) > 0 {
		httpx.WriteProblemFields(w, http.StatusBadRequest, "Please correct the highlighted fields", fields)
		return
	}

	var view DeploymentView
	var key any
	var secret string
	err = a.db.ExecTx(ctx, func(tx pgx.Tx) error {
		// A fully stopped deployment frees its name for reuse.
		if _, err := tx.Exec(ctx, `
			UPDATE deployments SET deleted_at = NOW()
			WHERE org_id = $1 AND name = $2 AND deleted_at IS NULL AND desired_state = 'stopped' AND state = 'stopped';
		`, claims.OrgID, req.Name); err != nil {
			return err
		}

		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO deployments (org_id, model_id, name, state, desired_state, tier, region, min_replicas, max_replicas,
			                         scale_to_zero, burst_to_spot, resident_in, quantization)
			VALUES ($1, $2, $3, 'pending', 'running', $4, $5, $6, $7, $8, $9, $10, NULLIF($11, ''))
			RETURNING id;
		`, claims.OrgID, req.ModelID, req.Name, req.Tier, req.Region, req.MinReplicas, req.MaxReplicas,
			req.ScaleToZero, req.BurstToSpot, req.ResidentIN, req.Quantization).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE deployments SET endpoint = $2 WHERE id = $1;`, id, a.endpointFor(r, id)); err != nil {
			return err
		}
		if err := lifecycle.Event(ctx, tx, id, nil, "state", lifecycle.Pending,
			fmt.Sprintf("Deployment created: %s on %s in %s", model.name, strings.ToUpper(req.Tier), req.Region)); err != nil {
			return err
		}

		// SRS FR-22: a one-time-visible API key scoped to this deployment.
		k, raw, err := a.createAPIKey(ctx, tx, claims.OrgID, "deployment:"+req.Name, &id)
		if err != nil {
			return err
		}
		key, secret = k, raw

		view, err = scanDeployment(tx.QueryRow(ctx, `SELECT `+deploymentColumns+`
			FROM deployments d JOIN models m ON m.id = d.model_id WHERE d.id = $1;`, id))
		return err
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpx.WriteProblemFields(w, http.StatusConflict, "A deployment with this name already exists",
				map[string]string{"name": "Pick a different name, or stop the existing deployment first"})
			return
		}
		a.log.Error("create deployment failed", "err", err)
		writeError(w, http.StatusInternalServerError, "Failed to create deployment")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"deployment": view,
		"api_key":    key,
		"secret":     secret,
		"base_url":   a.endpointFor(r, view.ID),
		"model":      view.Name,
	})
}

func presetExists(raw []byte, name string) bool {
	var presets []struct {
		Name string `json:"name"`
	}
	if err := jsonUnmarshal(raw, &presets); err != nil {
		return false
	}
	for _, p := range presets {
		if p.Name == name {
			return true
		}
	}
	return false
}

// HandleListDeployments lists the org's deployments with replica counts and
// 24-hour usage for the dashboard (SRS FR-80).
func (a *API) HandleListDeployments(w http.ResponseWriter, r *http.Request) {
	claims := claimsOf(r)
	includeStopped := r.URL.Query().Get("include_stopped") == "true"
	rows, err := a.db.Pool.Query(r.Context(), `
		SELECT `+deploymentColumns+`,
		       (SELECT COUNT(*) FROM replicas x WHERE x.deployment_id = d.id AND x.state = 'serving'),
		       (SELECT COUNT(*) FROM replicas x WHERE x.deployment_id = d.id AND x.state IN ('pending','pulling','loading','warming','serving','degraded'))
		FROM deployments d JOIN models m ON m.id = d.model_id
		WHERE d.org_id = $1 AND d.deleted_at IS NULL AND ($2 OR NOT (d.desired_state = 'stopped' AND d.state = 'stopped'))
		ORDER BY d.created_at DESC;
	`, claims.OrgID, includeStopped)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list deployments")
		return
	}
	defer rows.Close()

	list := []DeploymentView{}
	for rows.Next() {
		var serving, active int
		v, err := scanDeployment(rows, &serving, &active)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to read deployments")
			return
		}
		v.ReplicasServing, v.ReplicasActive = serving, active
		list = append(list, v)
	}
	rows.Close()

	for i := range list {
		u, err := a.usageSummary(r.Context(), claims.OrgID, list[i].ID, 24*time.Hour)
		if err == nil {
			list[i].Usage24h = u
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": list, "count": len(list)})
}

func (a *API) usageSummary(ctx context.Context, orgID, deploymentID string, window time.Duration) (*UsageSummary, error) {
	var u UsageSummary
	err := a.db.Pool.QueryRow(ctx, `
		SELECT COUNT(*), COUNT(*) FILTER (WHERE status <> 'success'),
		       COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0)
		FROM usage_events
		WHERE org_id = $1 AND ($2 = '' OR deployment_id::TEXT = $2) AND ts >= NOW() - ($3 * INTERVAL '1 second');
	`, orgID, deploymentID, int(window.Seconds())).Scan(&u.Requests, &u.Errors, &u.InputTokens, &u.OutputTokens)
	return &u, err
}

// HandleUsageDaily returns the organisation's requests and tokens per day for
// the last 30 days, oldest first, for the dashboard chart (SRS FR-80).
func (a *API) HandleUsageDaily(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Pool.Query(r.Context(), `
		SELECT DATE_TRUNC('day', ts) AS d, COUNT(*), COUNT(*) FILTER (WHERE status <> 'success'),
		       COALESCE(SUM(input_tokens + output_tokens), 0)
		FROM usage_events
		WHERE org_id = $1 AND ts >= NOW() - INTERVAL '30 days'
		GROUP BY d ORDER BY d;
	`, claimsOf(r).OrgID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read usage")
		return
	}
	defer rows.Close()
	type day struct {
		Date     string `json:"date"`
		Requests int64  `json:"requests"`
		Errors   int64  `json:"errors"`
		Tokens   int64  `json:"tokens"`
	}
	days := []day{}
	for rows.Next() {
		var d day
		var t time.Time
		if err := rows.Scan(&t, &d.Requests, &d.Errors, &d.Tokens); err == nil {
			d.Date = t.Format("2006-01-02")
			days = append(days, d)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days})
}

// loadDeployment fetches a deployment owned by the caller's org.
func (a *API) loadDeployment(r *http.Request) (DeploymentView, error) {
	return scanDeployment(a.db.Pool.QueryRow(r.Context(), `SELECT `+deploymentColumns+`
		FROM deployments d JOIN models m ON m.id = d.model_id
		WHERE d.id::TEXT = $1 AND d.org_id = $2 AND d.deleted_at IS NULL;`, r.PathValue("id"), claimsOf(r).OrgID))
}

// ReplicaView hides host identity from customers (Architecture §5: "hosts
// invisible to customers") while exposing what they need to reason about.
type ReplicaView struct {
	ID             string     `json:"id"`
	State          string     `json:"state"`
	Detail         *string    `json:"detail,omitempty"`
	LastError      *string    `json:"last_error,omitempty"`
	Tier           string     `json:"tier"`
	Region         string     `json:"region"`
	GPUModel       *string    `json:"gpu_model,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	StoppedAt      *time.Time `json:"stopped_at,omitempty"`
	TotalRequests  int64      `json:"total_requests"`
	FailedRequests int64      `json:"failed_requests"`
	CreatedAt      time.Time  `json:"created_at"`
}

func (a *API) replicas(ctx context.Context, deploymentID string, includeFinished bool) ([]ReplicaView, error) {
	rows, err := a.db.Pool.Query(ctx, `
		SELECT r.id, r.state, r.detail, r.last_error, h.tier, h.region, g.model, r.started_at, r.stopped_at,
		       r.total_requests, r.failed_requests, r.created_at
		FROM replicas r JOIN hosts h ON h.id = r.host_id LEFT JOIN gpus g ON g.id = r.gpu_id
		WHERE r.deployment_id = $1 AND ($2 OR r.state NOT IN ('stopped', 'failed') OR r.updated_at > NOW() - INTERVAL '1 hour')
		ORDER BY r.created_at DESC LIMIT 50;
	`, deploymentID, includeFinished)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReplicaView{}
	for rows.Next() {
		var x ReplicaView
		if err := rows.Scan(&x.ID, &x.State, &x.Detail, &x.LastError, &x.Tier, &x.Region, &x.GPUModel,
			&x.StartedAt, &x.StoppedAt, &x.TotalRequests, &x.FailedRequests, &x.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// HandleGetDeployment is the deployment detail view (PRD F-7): state, replicas,
// endpoint and usage.
func (a *API) HandleGetDeployment(w http.ResponseWriter, r *http.Request) {
	v, err := a.loadDeployment(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "Deployment not found")
		return
	}
	ctx := r.Context()
	reps, err := a.replicas(ctx, v.ID, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read replicas")
		return
	}
	day, _ := a.usageSummary(ctx, v.OrgID, v.ID, 24*time.Hour)
	hour, _ := a.usageSummary(ctx, v.OrgID, v.ID, time.Hour)

	writeJSON(w, http.StatusOK, map[string]any{
		"deployment":      v,
		"replicas":        reps,
		"usage_24h":       day,
		"usage_last_hour": hour,
		"base_url":        a.endpointFor(r, v.ID),
		"model":           v.Name,
	})
}

func (a *API) HandleDeploymentReplicas(w http.ResponseWriter, r *http.Request) {
	v, err := a.loadDeployment(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "Deployment not found")
		return
	}
	reps, err := a.replicas(r.Context(), v.ID, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read replicas")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"replicas": reps})
}

// HandleDeploymentLogs returns the deployment event log (FR-21), oldest first,
// after an optional cursor so the console can poll for new lines.
func (a *API) HandleDeploymentLogs(w http.ResponseWriter, r *http.Request) {
	v, err := a.loadDeployment(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "Deployment not found")
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	rows, err := a.db.Pool.Query(r.Context(), `
		SELECT id, replica_id, kind, state, message, created_at FROM (
			SELECT id, replica_id, kind, state, message, created_at FROM deployment_events
			WHERE deployment_id = $1 AND id > $2 ORDER BY id DESC LIMIT 200
		) e ORDER BY id ASC;
	`, v.ID, after)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read events")
		return
	}
	defer rows.Close()
	type event struct {
		ID        int64     `json:"id"`
		ReplicaID *string   `json:"replica_id,omitempty"`
		Kind      string    `json:"kind"`
		State     *string   `json:"state,omitempty"`
		Message   string    `json:"message"`
		CreatedAt time.Time `json:"created_at"`
	}
	events := []event{}
	for rows.Next() {
		var e event
		if err := rows.Scan(&e.ID, &e.ReplicaID, &e.Kind, &e.State, &e.Message, &e.CreatedAt); err == nil {
			events = append(events, e)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// HandleDeploymentMetrics returns time-bucketed usage for charts (PRD F-7:
// throughput, P95 latency and error rate over 24h/7d/30d).
func (a *API) HandleDeploymentMetrics(w http.ResponseWriter, r *http.Request) {
	v, err := a.loadDeployment(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "Deployment not found")
		return
	}
	window, bucket := 24*time.Hour, "hour"
	switch r.URL.Query().Get("window") {
	case "1h":
		window, bucket = time.Hour, "minute"
	case "7d":
		window, bucket = 7*24*time.Hour, "hour"
	case "30d":
		window, bucket = 30*24*time.Hour, "day"
	}
	rows, err := a.db.Pool.Query(r.Context(), `
		SELECT DATE_TRUNC($3, ts) AS t, COUNT(*), COUNT(*) FILTER (WHERE status <> 'success'),
		       COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		       COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY duration_ms), 0),
		       COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY duration_ms), 0)
		FROM usage_events
		WHERE deployment_id = $1 AND ts >= NOW() - ($2 * INTERVAL '1 second')
		GROUP BY t ORDER BY t;
	`, v.ID, int(window.Seconds()), bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read metrics")
		return
	}
	defer rows.Close()
	type point struct {
		T            time.Time `json:"t"`
		Requests     int64     `json:"requests"`
		Errors       int64     `json:"errors"`
		InputTokens  int64     `json:"input_tokens"`
		OutputTokens int64     `json:"output_tokens"`
		P50Ms        float64   `json:"p50_ms"`
		P95Ms        float64   `json:"p95_ms"`
	}
	points := []point{}
	for rows.Next() {
		var p point
		if err := rows.Scan(&p.T, &p.Requests, &p.Errors, &p.InputTokens, &p.OutputTokens, &p.P50Ms, &p.P95Ms); err == nil {
			points = append(points, p)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"bucket": bucket, "points": points})
}

type UpdateDeploymentRequest struct {
	// Action is pause, resume or retry (UML §4: PAUSED → SCHEDULING on resume,
	// FAILED → PENDING on retry).
	Action      string `json:"action"`
	MinReplicas *int   `json:"min_replicas"`
	MaxReplicas *int   `json:"max_replicas"`
}

// HandleUpdateDeployment implements PATCH /v1/deployments/:id (scale, pause, resume).
func (a *API) HandleUpdateDeployment(w http.ResponseWriter, r *http.Request) {
	v, err := a.loadDeployment(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "Deployment not found")
		return
	}
	var req UpdateDeploymentRequest
	if httpx.DecodeJSON(w, r, &req) != nil {
		return
	}
	ctx := r.Context()

	if v.DesiredState == lifecycle.DesiredStopped {
		writeError(w, http.StatusConflict, "This deployment has been stopped; create a new one instead")
		return
	}

	err = a.db.ExecTx(ctx, func(tx pgx.Tx) error {
		switch req.Action {
		case "":
		case "pause":
			if _, err := tx.Exec(ctx, `UPDATE deployments SET desired_state = 'paused', updated_at = NOW() WHERE id = $1;`, v.ID); err != nil {
				return err
			}
			if err := lifecycle.Event(ctx, tx, v.ID, nil, "info", "", "Pause requested"); err != nil {
				return err
			}
		case "resume", "retry":
			if req.Action == "resume" && v.DesiredState != lifecycle.DesiredPaused {
				return errBadAction("Only a paused deployment can be resumed")
			}
			if req.Action == "retry" && v.State != lifecycle.Failed {
				return errBadAction("Only a failed deployment can be retried")
			}
			if _, err := tx.Exec(ctx, `
				UPDATE deployments SET desired_state = 'running', state = 'pending', last_error = NULL,
				       state_changed_at = NOW(), updated_at = NOW()
				WHERE id = $1;
			`, v.ID); err != nil {
				return err
			}
			if err := lifecycle.Event(ctx, tx, v.ID, nil, "state", lifecycle.Pending, strings.ToUpper(req.Action[:1])+req.Action[1:]+" requested; waiting for capacity"); err != nil {
				return err
			}
		default:
			return errBadAction("action must be pause, resume or retry")
		}

		if req.MinReplicas != nil || req.MaxReplicas != nil {
			minR, maxR := v.MinReplicas, v.MaxReplicas
			if req.MinReplicas != nil {
				minR = *req.MinReplicas
			}
			if req.MaxReplicas != nil {
				maxR = *req.MaxReplicas
			}
			if minR < 1 || minR > maxReplicas || maxR < minR || maxR > maxReplicas {
				return errBadAction(fmt.Sprintf("replicas must satisfy 1 ≤ min ≤ max ≤ %d", maxReplicas))
			}
			if _, err := tx.Exec(ctx, `UPDATE deployments SET min_replicas = $2, max_replicas = $3, updated_at = NOW() WHERE id = $1;`, v.ID, minR, maxR); err != nil {
				return err
			}
			if err := lifecycle.Event(ctx, tx, v.ID, nil, "info", "", fmt.Sprintf("Scaled to %d–%d replicas", minR, maxR)); err != nil {
				return err
			}
		}
		return nil
	})
	var bad errBadAction
	if errors.As(err, &bad) {
		writeError(w, http.StatusConflict, string(bad))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update deployment")
		return
	}
	updated, _ := a.loadDeployment(r)
	writeJSON(w, http.StatusOK, map[string]any{"deployment": updated})
}

type errBadAction string

func (e errBadAction) Error() string { return string(e) }

// HandleStopDeployment implements DELETE /v1/deployments/:id. Replicas drain
// gracefully (UML §4 STOPPING → STOPPED); the row is kept for usage history.
func (a *API) HandleStopDeployment(w http.ResponseWriter, r *http.Request) {
	v, err := a.loadDeployment(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "Deployment not found")
		return
	}
	ctx := r.Context()
	err = a.db.ExecTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE deployments SET desired_state = 'stopped', updated_at = NOW() WHERE id = $1;`, v.ID); err != nil {
			return err
		}
		// Keys scoped to a stopped deployment are useless; revoke them.
		if _, err := tx.Exec(ctx, `UPDATE api_keys SET revoked = TRUE WHERE deployment_id = $1;`, v.ID); err != nil {
			return err
		}
		return lifecycle.Event(ctx, tx, v.ID, nil, "info", "", "Stop requested")
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to stop deployment")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "stopping", "id": v.ID})
}

// HandleCapacity previews placement for the deploy wizard: how many hosts could
// take the model on each tier right now (PRD F-3 "availability").
func (a *API) HandleCapacity(w http.ResponseWriter, r *http.Request) {
	claims := claimsOf(r)
	ctx := r.Context()
	q := r.URL.Query()

	var minVram int
	var isBYO bool
	var refsRaw []byte
	var tiers []string
	if err := a.db.Pool.QueryRow(ctx, `
		SELECT min_vram_gb, is_byo, runtime_refs, tiers_allowed FROM models
		WHERE id::TEXT = $1 AND (owner_org_id IS NULL OR owner_org_id = $2);
	`, q.Get("model_id"), claims.OrgID).Scan(&minVram, &isBYO, &refsRaw, &tiers); err != nil {
		writeError(w, http.StatusNotFound, "Model not found")
		return
	}
	refs, _ := placement.ParseRuntimeRefs(refsRaw)
	region := q.Get("region")
	if region == "" {
		region = domain.RegionInSouth
	}

	cands, err := placement.LoadCandidates(ctx, a.db.Pool, a.heartbeatTimeout)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read capacity")
		return
	}
	type tierCap struct {
		Tier          string `json:"tier"`
		Allowed       bool   `json:"allowed"`
		EligibleHosts int    `json:"eligible_hosts"`
		Message       string `json:"message,omitempty"`
	}
	out := []tierCap{}
	for _, t := range []string{domain.TierT1, domain.TierT2, domain.TierT3} {
		tc := tierCap{Tier: t, Allowed: slices.Contains(tiers, t) && !(isBYO && t == domain.TierT3)}
		if tc.Allowed {
			req := placement.Requirements{Tier: t, Region: region, IsBYO: isBYO, RuntimeRefs: refs, FallbackMinVramGB: minVram}
			ranked, rej := placement.Rank(cands, req)
			tc.EligibleHosts = len(ranked)
			if len(ranked) == 0 {
				tc.Message = placement.NoCapacityMessage(req, rej, len(cands))
			}
		}
		out = append(out, tc)
	}
	writeJSON(w, http.StatusOK, map[string]any{"region": region, "tiers": out, "online_gpus": len(cands)})
}
