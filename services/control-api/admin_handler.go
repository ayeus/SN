package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/ayeus/ayeusann/internal/domain"
	"github.com/ayeus/ayeusann/internal/httpx"
	"github.com/ayeus/ayeusann/internal/lifecycle"
	"github.com/jackc/pgx/v5"
)

// Every ops action is written to the audit log (NFR-16).
func (a *API) audit(r *http.Request, action, resourceType, resourceID string, details map[string]any) {
	c := claimsOf(r)
	_, err := a.db.Pool.Exec(r.Context(), `
		INSERT INTO audit_log (actor_id, actor_type, action, resource_type, resource_id, details_json, user_agent)
		VALUES ($1, 'admin', $2, $3, $4::UUID, $5, $6);
	`, c.UserID, action, resourceType, resourceID, details, r.UserAgent())
	if err != nil {
		a.log.Error("audit log write failed", "action", action, "err", err)
	}
}

// HandleAdminFleet is the fleet view: every host with owner, liveness and load.
func (a *API) HandleAdminFleet(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Pool.Query(r.Context(), `
		SELECT `+hostColumns+`, u.email,
		       (SELECT COUNT(*) FROM gpus g WHERE g.host_id = h.id),
		       (SELECT COUNT(*) FROM replicas x WHERE x.host_id = h.id AND x.state IN ('pending','pulling','loading','warming','serving','degraded'))
		FROM hosts h LEFT JOIN users u ON u.id = h.user_id
		WHERE h.deleted_at IS NULL
		ORDER BY h.last_heartbeat_at DESC NULLS LAST LIMIT 500;
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read fleet")
		return
	}
	defer rows.Close()
	fleet := []map[string]any{}
	for rows.Next() {
		var email *string
		var gpus, jobs int
		h, err := scanHost(rows, &email, &gpus, &jobs)
		if err != nil {
			continue
		}
		fleet = append(fleet, map[string]any{"host": h, "online": a.online(h), "owner_email": email, "gpus": gpus, "active_jobs": jobs})
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": fleet, "count": len(fleet)})
}

func (a *API) HandleAdminIncidents(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Pool.Query(r.Context(), `
		SELECT i.id, i.host_id, h.name, i.kind, i.severity, i.action, i.evidence_json, i.resolved, i.created_at
		FROM trust_incidents i JOIN hosts h ON h.id = i.host_id
		ORDER BY i.resolved, i.created_at DESC LIMIT 200;
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read incidents")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, hostID, name, kind, sev string
		var action *string
		var evidence []byte
		var resolved bool
		var at time.Time
		if err := rows.Scan(&id, &hostID, &name, &kind, &sev, &action, &evidence, &resolved, &at); err == nil {
			out = append(out, map[string]any{"id": id, "host_id": hostID, "host_name": name, "kind": kind, "severity": sev,
				"action": action, "evidence": rawJSON(evidence), "resolved": resolved, "created_at": at})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"incidents": out})
}

// HandleAdminDrainHost moves a host's work elsewhere for maintenance.
func (a *API) HandleAdminDrainHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	err := a.db.ExecTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE hosts SET paused = TRUE, status = CASE WHEN status IN ('active','probation') THEN 'draining' ELSE status END,
			       updated_at = NOW()
			WHERE id::TEXT = $1 AND deleted_at IS NULL;
		`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return drainHostReplicas(ctx, tx, id, "host drained by operations")
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "Host not found")
		return
	}
	a.audit(r, "host.drain", "host", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "draining", "id": id})
}

// HandleAdminBanHost is the per-host kill switch (Architecture §6): the host's
// credential is revoked, its jobs are failed over, and it can never re-enrol
// with the same hardware.
func (a *API) HandleAdminBanHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Reason string `json:"reason"`
	}
	_ = httpxDecodeOptional(r, &req)
	ctx := r.Context()
	err := a.db.ExecTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE hosts SET status = 'banned', credential_hash = NULL, paused = TRUE, updated_at = NOW()
			WHERE id::TEXT = $1 AND deleted_at IS NULL;
		`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		rows, err := tx.Query(ctx, `SELECT id FROM replicas WHERE host_id::TEXT = $1 AND state NOT IN ('stopped', 'failed');`, id)
		if err != nil {
			return err
		}
		var reps []string
		for rows.Next() {
			var rid string
			if err := rows.Scan(&rid); err == nil {
				reps = append(reps, rid)
			}
		}
		rows.Close()
		deps := map[string]bool{}
		for _, rid := range reps {
			d, _, err := lifecycle.SetReplicaState(ctx, tx, rid, lifecycle.Failed, "", "host removed from the network")
			if err != nil {
				return err
			}
			deps[d] = true
		}
		for d := range deps {
			if _, _, err := lifecycle.Recompute(ctx, tx, d); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO trust_incidents (host_id, kind, severity, action, evidence_json)
			VALUES ($1::UUID, 'abuse', 'critical', 'ban', jsonb_build_object('reason', $2::TEXT));
		`, id, req.Reason)
		return err
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "Host not found")
		return
	}
	a.audit(r, "host.ban", "host", id, map[string]any{"reason": req.Reason})
	writeJSON(w, http.StatusOK, map[string]string{"status": "banned", "id": id})
}

// HandleAdminSetHostTier approves a host into a tier (the T2 review queue and
// T1 contract onboarding of PRD F-10).
func (a *API) HandleAdminSetHostTier(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Tier string `json:"tier"`
	}
	if httpx.DecodeJSON(w, r, &req) != nil {
		return
	}
	req.Tier = strings.ToLower(req.Tier)
	if req.Tier != domain.TierT1 && req.Tier != domain.TierT2 && req.Tier != domain.TierT3 {
		writeError(w, http.StatusBadRequest, "tier must be t1, t2 or t3")
		return
	}
	tag, err := a.db.Pool.Exec(r.Context(), `
		UPDATE hosts SET tier = $2,
		       status = CASE WHEN status = 'probation' AND $2 = 't1' THEN 'active' ELSE status END,
		       updated_at = NOW()
		WHERE id::TEXT = $1 AND deleted_at IS NULL;
	`, id, req.Tier)
	if err != nil || tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "Host not found")
		return
	}
	a.audit(r, "host.set_tier", "host", id, map[string]any{"tier": req.Tier})
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated", "tier": req.Tier})
}

// HandleAdminKillDeployment is the per-deployment kill switch.
func (a *API) HandleAdminKillDeployment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	err := a.db.ExecTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE deployments SET desired_state = 'stopped', updated_at = NOW() WHERE id::TEXT = $1;`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return lifecycle.Event(ctx, tx, id, nil, "error", "", "Stopped by platform operations")
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "Deployment not found")
		return
	}
	a.audit(r, "deployment.kill", "deployment", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopping", "id": id})
}
