package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/spazor/spazenode/internal/auth"
	"github.com/spazor/spazenode/internal/db"
	"github.com/spazor/spazenode/internal/domain"
)

type ModelHandler struct {
	db *db.Client
}

func NewModelHandler(database *db.Client) *ModelHandler {
	return &ModelHandler{db: database}
}

type BYOModelRequest struct {
	Name            string   `json:"name"`
	Family          string   `json:"family"`
	ParamsB         float32  `json:"params_b"`
	License         string   `json:"license"`
	MinVramGB       int      `json:"min_vram_gb"`
	TiersAllowed    []string `json:"tiers_allowed"`
	HuggingFaceURL  string   `json:"huggingface_url,omitempty"`
	PriceInPer1M    float64  `json:"price_in_per_1m"`
	PriceOutPer1M   float64  `json:"price_out_per_1m"`
}

func (h *ModelHandler) HandleListModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	queryValues := r.URL.Query()

	tierFilter := queryValues.Get("tier")
	minVramFilter := queryValues.Get("min_vram_gb")

	baseQuery := `
		SELECT id, name, family, params_b, license, min_vram_gb, tiers_allowed,
		       price_in_per_1m, price_out_per_1m, price_per_hour_inr, is_byo,
		       quantization_presets, created_at, updated_at
		FROM models
		WHERE 1=1
	`

	args := []interface{}{}
	argIdx := 1

	if tierFilter != "" {
		baseQuery += fmt.Sprintf(" AND $%d = ANY(tiers_allowed)", argIdx)
		args = append(args, strings.ToLower(tierFilter))
		argIdx++
	}

	if minVramFilter != "" {
		if vram, err := strconv.Atoi(minVramFilter); err == nil {
			baseQuery += fmt.Sprintf(" AND min_vram_gb <= $%d", argIdx)
			args = append(args, vram)
			argIdx++
		}
	}

	baseQuery += " ORDER BY params_b ASC, name ASC;"

	rows, err := h.db.Pool.Query(ctx, baseQuery, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to query models")
		return
	}
	defer rows.Close()

	models := []domain.Model{}
	for rows.Next() {
		var m domain.Model
		err := rows.Scan(
			&m.ID, &m.Name, &m.Family, &m.ParamsB, &m.License, &m.MinVramGB, &m.TiersAllowed,
			&m.PriceInPer1M, &m.PriceOutPer1M, &m.PricePerHourINR, &m.IsBYO,
			&m.QuantizationPresets, &m.CreatedAt, &m.UpdatedAt,
		)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to scan model row")
			return
		}
		models = append(models, m)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"models": models,
		"count":  len(models),
	})
}

func (h *ModelHandler) HandleGetModel(w http.ResponseWriter, r *http.Request) {
	modelID := strings.TrimPrefix(r.URL.Path, "/v1/models/")
	if modelID == "" {
		writeError(w, http.StatusBadRequest, "Model ID required")
		return
	}

	ctx := r.Context()
	var m domain.Model

	var query string
	var args []interface{}

	if _, err := uuid.Parse(modelID); err == nil {
		query = `
			SELECT id, name, family, params_b, license, min_vram_gb, tiers_allowed,
			       price_in_per_1m, price_out_per_1m, price_per_hour_inr, is_byo,
			       quantization_presets, created_at, updated_at
			FROM models
			WHERE id = $1 OR name = $1;
		`
		args = append(args, modelID)
	} else {
		query = `
			SELECT id, name, family, params_b, license, min_vram_gb, tiers_allowed,
			       price_in_per_1m, price_out_per_1m, price_per_hour_inr, is_byo,
			       quantization_presets, created_at, updated_at
			FROM models
			WHERE name = $1;
		`
		args = append(args, modelID)
	}

	err := h.db.Pool.QueryRow(ctx, query, args...).Scan(
		&m.ID, &m.Name, &m.Family, &m.ParamsB, &m.License, &m.MinVramGB, &m.TiersAllowed,
		&m.PriceInPer1M, &m.PriceOutPer1M, &m.PricePerHourINR, &m.IsBYO,
		&m.QuantizationPresets, &m.CreatedAt, &m.UpdatedAt,
	)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "Model not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "Database error")
		return
	}

	// Fetch artifacts
	artQuery := `
		SELECT id, model_id, version, artifact_url, size_bytes, checksum, format, created_at
		FROM model_artifacts
		WHERE model_id = $1;
	`
	rows, err := h.db.Pool.Query(ctx, artQuery, m.ID)
	var artifacts []domain.ModelArtifact
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var a domain.ModelArtifact
			if err := rows.Scan(&a.ID, &a.ModelID, &a.Version, &a.ArtifactURL, &a.SizeBytes, &a.Checksum, &a.Format, &a.CreatedAt); err == nil {
				artifacts = append(artifacts, a)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"model":     m,
		"artifacts": artifacts,
	})
}

func (h *ModelHandler) HandleBYOModel(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.GetClaims(r.Context())
	if !ok || claims == nil {
		writeError(w, http.StatusUnauthorized, "Unauthenticated")
		return
	}

	var req BYOModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if req.Name == "" || req.Family == "" || req.MinVramGB <= 0 {
		writeError(w, http.StatusBadRequest, "Name, family, and positive min_vram_gb are required")
		return
	}

	if len(req.TiersAllowed) == 0 {
		// BYO models default to T1 and T2 only per security policy (ADR-007)
		req.TiersAllowed = []string{domain.TierT1, domain.TierT2}
	} else {
		// Enforce Security Policy: BYO models are NEVER allowed on T3 spot hosts
		for _, t := range req.TiersAllowed {
			if strings.EqualFold(t, domain.TierT3) {
				writeError(w, http.StatusForbidden, "Security Policy Violation: BYO models are not permitted on Tier 3 (T3) personal hosts")
				return
			}
		}
	}

	ctx := r.Context()
	var m domain.Model
	query := `
		INSERT INTO models (
			name, family, params_b, license, min_vram_gb, tiers_allowed,
			price_in_per_1m, price_out_per_1m, is_byo, quantization_presets
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, TRUE, '[]'::jsonb)
		RETURNING id, name, family, params_b, license, min_vram_gb, tiers_allowed,
		          price_in_per_1m, price_out_per_1m, is_byo, created_at, updated_at;
	`
	err := h.db.Pool.QueryRow(ctx, query,
		req.Name, req.Family, req.ParamsB, req.License, req.MinVramGB, req.TiersAllowed,
		req.PriceInPer1M, req.PriceOutPer1M,
	).Scan(
		&m.ID, &m.Name, &m.Family, &m.ParamsB, &m.License, &m.MinVramGB, &m.TiersAllowed,
		&m.PriceInPer1M, &m.PriceOutPer1M, &m.IsBYO, &m.CreatedAt, &m.UpdatedAt,
	)

	if err != nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("Failed to register BYO model (name may exist): %v", err))
		return
	}

	// Create artifact record if Hugging Face URL provided
	if req.HuggingFaceURL != "" {
		artQuery := `
			INSERT INTO model_artifacts (model_id, version, artifact_url, size_bytes, checksum, format)
			VALUES ($1, 'v1.0', $2, 0, 'sha256:byo_huggingface', 'safetensors');
		`
		_, _ = h.db.Pool.Exec(ctx, artQuery, m.ID, req.HuggingFaceURL)
	}

	writeJSON(w, http.StatusCreated, m)
}
