package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/ayeus/ayeusann/internal/domain"
	"github.com/ayeus/ayeusann/internal/httpx"
	"github.com/ayeus/ayeusann/internal/money"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// platformCurrency is the currency catalogue token prices are quoted in.
const platformCurrency = "USD"

// modelSelect is the column list every model read shares.
const modelSelect = `
	SELECT id, name, family, params_b, license, min_vram_gb, tiers_allowed,
	       price_in_per_1m, price_out_per_1m, price_per_hour_inr, is_byo,
	       quantization_presets, runtime_refs, description, context_length, created_at, updated_at
	FROM models`

func scanModel(row pgx.Row) (domain.Model, error) {
	var m domain.Model
	err := row.Scan(
		&m.ID, &m.Name, &m.Family, &m.ParamsB, &m.License, &m.MinVramGB, &m.TiersAllowed,
		&m.PriceInPer1M, &m.PriceOutPer1M, &m.PricePerHourINR, &m.IsBYO,
		&m.QuantizationPresets, &m.RuntimeRefs, &m.Description, &m.ContextLength, &m.CreatedAt, &m.UpdatedAt,
	)
	m.PriceInPer1M = money.FromMicros(m.PriceInPer1M.Micros(), platformCurrency)
	m.PriceOutPer1M = money.FromMicros(m.PriceOutPer1M.Micros(), platformCurrency)
	if m.PricePerHourINR != nil {
		v := money.FromMicros(m.PricePerHourINR.Micros(), "INR")
		m.PricePerHourINR = &v
	}
	return m, err
}

// httpxDecodeOptional decodes a body that may legitimately be absent.
func httpxDecodeOptional(r *http.Request, dst any) error {
	if r.Body == nil {
		return io.EOF
	}
	return json.NewDecoder(io.LimitReader(r.Body, httpx.MaxBodyBytes)).Decode(dst)
}

// modelNamePattern keeps model names URL- and path-safe: they end up in
// endpoint URLs and, for BYO models, in paths on host machines.
var modelNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,62}[a-z0-9]$`)

// maxTokenPrice caps a BYO price so a typo cannot become an invoice.
var maxTokenPrice = money.MustParse("1000", "USD")

var sha256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// HandleListModels serves the public catalogue plus the caller's own BYO
// models (PRD F-2).
func (a *API) HandleListModels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := modelSelect + ` WHERE owner_org_id IS NULL`
	var args []any
	if c := claimsOf(r); c != nil && c.OrgID != "" {
		query = modelSelect + ` WHERE (owner_org_id IS NULL OR owner_org_id = $1)`
		args = append(args, c.OrgID)
	}

	if tier := strings.ToLower(strings.TrimSpace(q.Get("tier"))); tier != "" {
		if tier != domain.TierT1 && tier != domain.TierT2 && tier != domain.TierT3 {
			writeError(w, http.StatusBadRequest, "tier must be one of t1, t2, t3")
			return
		}
		args = append(args, tier)
		query += fmt.Sprintf(" AND $%d = ANY(tiers_allowed)", len(args))
	}
	if raw := q.Get("min_vram_gb"); raw != "" {
		vram, err := strconv.Atoi(raw)
		if err != nil || vram <= 0 {
			writeError(w, http.StatusBadRequest, "min_vram_gb must be a positive integer")
			return
		}
		args = append(args, vram)
		query += fmt.Sprintf(" AND min_vram_gb <= $%d", len(args))
	}
	query += " ORDER BY is_byo, params_b, name LIMIT 500;"

	rows, err := a.db.Pool.Query(r.Context(), query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to query models")
		return
	}
	defer rows.Close()
	models := []domain.Model{}
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to read models")
			return
		}
		models = append(models, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models, "count": len(models)})
}

// HandleGetModel returns one model by id or name, scoped to the catalogue plus
// the caller's own models.
func (a *API) HandleGetModel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	args := []any{id}
	scope := "owner_org_id IS NULL"
	if c := claimsOf(r); c != nil && c.OrgID != "" {
		args = append(args, c.OrgID)
		scope = "(owner_org_id IS NULL OR owner_org_id = $2)"
	}
	match := "name = $1"
	if _, err := uuid.Parse(id); err == nil {
		match = "(name = $1 OR id = $1::uuid)"
	}
	m, err := scanModel(a.db.Pool.QueryRow(r.Context(),
		modelSelect+` WHERE `+scope+` AND `+match+` ORDER BY owner_org_id NULLS LAST LIMIT 1;`, args...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "Model not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "Database error")
		return
	}

	// Only verified artifacts are listed; an unverified row describes bytes
	// nobody has hashed.
	artifacts := []domain.ModelArtifact{}
	rows, err := a.db.Pool.Query(r.Context(), `
		SELECT id, model_id, version, artifact_url, size_bytes, checksum, format, created_at
		FROM model_artifacts WHERE model_id = $1 AND verified ORDER BY created_at DESC;
	`, m.ID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var x domain.ModelArtifact
			if err := rows.Scan(&x.ID, &x.ModelID, &x.Version, &x.ArtifactURL, &x.SizeBytes, &x.Checksum, &x.Format, &x.CreatedAt); err == nil {
				artifacts = append(artifacts, x)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": m, "artifacts": artifacts})
}

type BYOModelRequest struct {
	Name          string           `json:"name"`
	Family        string           `json:"family"`
	ParamsB       float32          `json:"params_b"`
	License       string           `json:"license"`
	MinVramGB     int              `json:"min_vram_gb"`
	TiersAllowed  []string         `json:"tiers_allowed"`
	PriceInPer1M  money.Amount     `json:"price_in_per_1m"`
	PriceOutPer1M money.Amount     `json:"price_out_per_1m"`
	Artifact      *ArtifactRequest `json:"artifact,omitempty"`
}

type ArtifactRequest struct {
	Version   string `json:"version"`
	URL       string `json:"url"`
	SizeBytes int64  `json:"size_bytes"`
	Checksum  string `json:"checksum"`
	Format    string `json:"format"`
}

func (req *BYOModelRequest) validate() error {
	req.Name = strings.ToLower(strings.TrimSpace(req.Name))
	req.Family = strings.ToLower(strings.TrimSpace(req.Family))
	req.License = strings.TrimSpace(req.License)
	switch {
	case !modelNamePattern.MatchString(req.Name):
		return errors.New("name must be 3-64 characters of lowercase letters, digits, dot, dash or underscore")
	case req.Family == "":
		return errors.New("family is required")
	case req.License == "":
		return errors.New("license is required; the platform cannot serve weights of unknown licensing")
	case req.ParamsB <= 0 || req.ParamsB > 100_000:
		return errors.New("params_b must be between 0 and 100000")
	case req.MinVramGB <= 0 || req.MinVramGB > 100_000:
		return errors.New("min_vram_gb must be between 1 and 100000")
	}
	req.PriceInPer1M = money.FromMicros(req.PriceInPer1M.Micros(), platformCurrency)
	req.PriceOutPer1M = money.FromMicros(req.PriceOutPer1M.Micros(), platformCurrency)
	for label, p := range map[string]money.Amount{"price_in_per_1m": req.PriceInPer1M, "price_out_per_1m": req.PriceOutPer1M} {
		if p.IsNegative() || p.GreaterThan(maxTokenPrice) {
			return fmt.Errorf("%s must be between 0 and %s", label, maxTokenPrice.Display())
		}
	}
	if art := req.Artifact; art != nil {
		art.Version = strings.TrimSpace(art.Version)
		art.URL = strings.TrimSpace(art.URL)
		art.Checksum = strings.ToLower(strings.TrimSpace(art.Checksum))
		art.Format = strings.ToLower(strings.TrimSpace(art.Format))
		if art.Version == "" {
			art.Version = "v1"
		}
		if art.Format == "" {
			art.Format = "safetensors"
		}
		switch {
		case !strings.HasPrefix(art.URL, "https://") && !strings.HasPrefix(art.URL, "s3://"):
			return errors.New("artifact.url must be an https:// or s3:// URL")
		case !sha256Pattern.MatchString(art.Checksum):
			return errors.New(`artifact.checksum must be "sha256:" followed by 64 lowercase hex characters`)
		case art.SizeBytes <= 0:
			return errors.New("artifact.size_bytes must be positive")
		}
		switch art.Format {
		case "safetensors", "gguf", "gptq", "awq":
		default:
			return errors.New("artifact.format must be one of safetensors, gguf, gptq, awq")
		}
	}
	return nil
}

// HandleBYOModel registers a bring-your-own model (PRD F-2, SRS FR-11). BYO
// weights are restricted to T1/T2 (ADR-007, NFR-12).
func (a *API) HandleBYOModel(w http.ResponseWriter, r *http.Request) {
	claims := claimsOf(r)
	var req BYOModelRequest
	if httpx.DecodeJSON(w, r, &req) != nil {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	tiers := []string{domain.TierT1, domain.TierT2}
	if len(req.TiersAllowed) > 0 {
		tiers = tiers[:0]
		for _, t := range req.TiersAllowed {
			switch strings.ToLower(strings.TrimSpace(t)) {
			case domain.TierT1, domain.TierT2:
				tiers = append(tiers, strings.ToLower(strings.TrimSpace(t)))
			case domain.TierT3:
				writeError(w, http.StatusForbidden, "BYO models are not permitted on Tier 3 (personal) hosts")
				return
			default:
				writeError(w, http.StatusBadRequest, "tiers_allowed must contain only t1 or t2")
				return
			}
		}
	}

	ctx := r.Context()
	var m domain.Model
	err := a.db.ExecTx(ctx, func(tx pgx.Tx) error {
		var err error
		m, err = scanModel(tx.QueryRow(ctx, `
			INSERT INTO models (name, family, params_b, license, min_vram_gb, tiers_allowed,
			                    price_in_per_1m, price_out_per_1m, is_byo, owner_org_id, quantization_presets)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, TRUE, $9, '[]'::jsonb)
			RETURNING id, name, family, params_b, license, min_vram_gb, tiers_allowed,
			          price_in_per_1m, price_out_per_1m, price_per_hour_inr, is_byo,
			          quantization_presets, runtime_refs, description, context_length, created_at, updated_at;
		`, req.Name, req.Family, req.ParamsB, req.License, req.MinVramGB, tiers,
			req.PriceInPer1M, req.PriceOutPer1M, claims.OrgID))
		if err != nil {
			return err
		}
		if art := req.Artifact; art != nil {
			// Recorded unverified: ingest downloads and hashes the bytes before
			// any deployment may use them.
			_, err = tx.Exec(ctx, `
				INSERT INTO model_artifacts (model_id, version, artifact_url, size_bytes, checksum, format, verified)
				VALUES ($1, $2, $3, $4, $5, $6, FALSE);
			`, m.ID, art.Version, art.URL, art.SizeBytes, art.Checksum, art.Format)
		}
		return err
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "A model with that name already exists in this organisation")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to register model")
		return
	}

	status, msg := "none", "Model registered. Add a verified artifact before deploying it."
	if req.Artifact != nil {
		status, msg = "pending_verification", "Artifact registered. It will be downloaded and its digest checked before any deployment can use it."
	}
	writeJSON(w, http.StatusCreated, map[string]any{"model": m, "artifact_status": status, "message": msg})
}
