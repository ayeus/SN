package main

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/domain"
)

type HostHandler struct {
	db *db.Client
	tm *auth.TokenManager
}

func NewHostHandler(database *db.Client, tm *auth.TokenManager) *HostHandler {
	return &HostHandler{db: database, tm: tm}
}

type IssueTokenResponse struct {
	RegistrationToken string    `json:"registration_token"`
	ExpiresAt         time.Time `json:"expires_at"`
}

// HandleIssueRegistrationToken generates a 24-hour registration token for onboarding host machines.
func (h *HostHandler) HandleIssueRegistrationToken(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.GetClaims(r.Context())
	if !ok || claims == nil {
		writeError(w, http.StatusUnauthorized, "Unauthenticated")
		return
	}

	// Issue a single-use registration JWT token valid for 24 hours.
	regTok, err := h.tm.GenerateRegistrationToken(claims.UserID, claims.OrgID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to issue registration token")
		return
	}

	writeJSON(w, http.StatusCreated, IssueTokenResponse{
		RegistrationToken: regTok.Token,
		ExpiresAt:         regTok.ExpiresAt,
	})
}

// HandleListHosts returns all hosts belonging to the organization or user.
func (h *HostHandler) HandleListHosts(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.GetClaims(r.Context())
	if !ok || claims == nil {
		writeError(w, http.StatusUnauthorized, "Unauthenticated")
		return
	}

	ctx := r.Context()
	query := `
		SELECT id, user_id, name, hostname, tier, region, overlay_ip, kyc_status,
		       reputation, status, hw_fingerprint, agent_version, last_heartbeat_at,
		       created_at, updated_at
		FROM hosts
		WHERE user_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC;
	`

	rows, err := h.db.Pool.Query(ctx, query, claims.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to query hosts")
		return
	}
	defer rows.Close()

	hosts := []domain.Host{}
	for rows.Next() {
		var host domain.Host
		err := rows.Scan(
			&host.ID, &host.UserID, &host.Name, &host.Hostname, &host.Tier, &host.Region,
			&host.OverlayIP, &host.KycStatus, &host.Reputation, &host.Status, &host.HwFingerprint,
			&host.AgentVersion, &host.LastHeartbeatAt, &host.CreatedAt, &host.UpdatedAt,
		)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to scan host row")
			return
		}
		hosts = append(hosts, host)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"hosts": hosts,
		"count": len(hosts),
	})
}

// HandleGetHost returns detailed info for a specific host including its GPUs.
func (h *HostHandler) HandleGetHost(w http.ResponseWriter, r *http.Request) {
	hostID := strings.TrimPrefix(r.URL.Path, "/v1/hosts/")
	if hostID == "" {
		writeError(w, http.StatusBadRequest, "Host ID required")
		return
	}

	if _, err := uuid.Parse(hostID); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid Host ID format")
		return
	}

	ctx := r.Context()
	var host domain.Host
	query := `
		SELECT id, user_id, name, hostname, tier, region, overlay_ip, kyc_status,
		       reputation, status, hw_fingerprint, agent_version, last_heartbeat_at,
		       created_at, updated_at
		FROM hosts
		WHERE id = $1 AND deleted_at IS NULL;
	`
	err := h.db.Pool.QueryRow(ctx, query, hostID).Scan(
		&host.ID, &host.UserID, &host.Name, &host.Hostname, &host.Tier, &host.Region,
		&host.OverlayIP, &host.KycStatus, &host.Reputation, &host.Status, &host.HwFingerprint,
		&host.AgentVersion, &host.LastHeartbeatAt, &host.CreatedAt, &host.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "Host not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "Database error")
		return
	}

	// Fetch GPUs belonging to this host
	gpuQuery := `
		SELECT id, host_id, model, vram_gb, driver_version, cuda_version, uuid, fingerprint, status, created_at
		FROM gpus
		WHERE host_id = $1;
	`
	rows, err := h.db.Pool.Query(ctx, gpuQuery, host.ID)
	var gpus []domain.GPU
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var g domain.GPU
			if err := rows.Scan(&g.ID, &g.HostID, &g.Model, &g.VramGB, &g.DriverVersion, &g.CudaVersion, &g.UUID, &g.Fingerprint, &g.Status, &g.CreatedAt); err == nil {
				gpus = append(gpus, g)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"host": host,
		"gpus": gpus,
	})
}
