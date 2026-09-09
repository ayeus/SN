package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/domain"
	"github.com/ayeus/ayeusann/internal/money"
)

// platformCurrency is the base currency for all model pricing. It is USD because
// the catalog prices are denominated in USD and converted to the customer's
// billing currency at invoice time.
const platformCurrency = "USD"

// decodeJSON reads the request body as JSON into dst, returning a 400 error to
// the client when the body is malformed.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst interface{}) error {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return err
	}
	return nil
}

// isUniqueViolation reports whether err is a Postgres unique constraint violation
// (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

type ModelHandler struct {
	db *db.Client
}

func NewModelHandler(database *db.Client) *ModelHandler {
	return &ModelHandler{db: database}
}

// modelSelect is the column list every model read shares, so a change to the
// struct cannot silently desynchronize one query from another.
const modelSelect = `
	SELECT id, name, family, params_b, license, min_vram_gb, tiers_allowed,
	       price_in_per_1m, price_out_per_1m, price_per_hour_inr, is_byo,
	       quantization_presets, created_at, updated_at
	FROM models`

// modelNamePattern restricts model names to a URL- and path-safe slug. Names
// end up in endpoint URLs and, for BYO models, in filesystem paths on host
// machines, so anything else is an injection surface.
var modelNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,62}[a-z0-9]$`)

// maxTokenPrice caps a BYO price at $1000 per million tokens. Without a ceiling
// a typo turns into an invoice, and the NUMERIC(12,4) column would overflow.
var maxTokenPrice = money.MustParse("1000", "USD")

// sha256Pattern matches the only checksum form the artifact table accepts.
var sha256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type BYOModelRequest struct {
	Name         string   `json:"name"`
	Family       string   `json:"family"`
	ParamsB      float32  `json:"params_b"`
	License      string   `json:"license"`
	MinVramGB    int      `json:"min_vram_gb"`
	TiersAllowed []string `json:"tiers_allowed"`

	// PriceInPer1M and PriceOutPer1M are exact decimals in USD. They accept a
	// JSON string or number; a string is preferred because a JSON number is a
	// float64 in most clients.
	PriceInPer1M  money.Amount `json:"price_in_per_1m"`
	PriceOutPer1M money.Amount `json:"price_out_per_1m"`

	// Artifact describes where the weights live. It is optional; a model with no
	// artifact is registered but cannot be deployed until one is ingested and
	// verified. The previous version accepted a bare Hugging Face URL and wrote
	// a fabricated checksum, which made verification meaningless.
	Artifact *ArtifactRequest `json:"artifact,omitempty"`
}

type ArtifactRequest struct {
	Version   string `json:"version"`
	URL       string `json:"url"`
	SizeBytes int64  `json:"size_bytes"`
	// Checksum must be "sha256:" followed by 64 lowercase hex characters. It is
	// the digest of the artifact the host will download, and the host refuses to
	// load weights whose digest does not match.
	Checksum string `json:"checksum"`
	Format    string `json:"format"`
}

func (r *BYOModelRequest) validate() error {
	r.Name = strings.ToLower(strings.TrimSpace(r.Name))
	r.Family = strings.ToLower(strings.TrimSpace(r.Family))
	r.License = strings.TrimSpace(r.License)

	if !modelNamePattern.MatchString(r.Name) {
		return errors.New("name must be 3-64 characters of lowercase letters, digits, dot, dash or underscore")
	}
	if r.Family == "" {
		return errors.New("family is required")
	}
	if r.License == "" {
		return errors.New("license is required; the platform cannot serve weights of unknown licensing")
	}
	if r.ParamsB <= 0 || r.ParamsB > 100_000 {
		return errors.New("params_b must be between 0 and 100000")
	}
	if r.MinVramGB <= 0 || r.MinVramGB > 100_000 {
		return errors.New("min_vram_gb must be between 1 and 100000")
	}

	// Prices arrive without a currency because the wire format carries none.
	r.PriceInPer1M = money.FromMicros(r.PriceInPer1M.Micros(), platformCurrency)
	r.PriceOutPer1M = money.FromMicros(r.PriceOutPer1M.Micros(), platformCurrency)

	for label, p := range map[string]money.Amount{
		"price_in_per_1m":  r.PriceInPer1M,
		"price_out_per_1m": r.PriceOutPer1M,
	} {
		if p.IsNegative() {
			return fmt.Errorf("%s must not be negative", label)
		}
		if p.GreaterThan(maxTokenPrice) {
			return fmt.Errorf("%s must not exceed %s", label, maxTokenPrice.Display())
		}
	}

	if r.Artifact != nil {
		a := r.Artifact
		a.Version = strings.TrimSpace(a.Version)
		a.URL = strings.TrimSpace(a.URL)
		a.Checksum = strings.ToLower(strings.TrimSpace(a.Checksum))
		a.Format = strings.ToLower(strings.TrimSpace(a.Format))

		if a.Version == "" {
			a.Version = "v1"
		}
		if a.URL == "" {
			return errors.New("artifact.url is required when an artifact is supplied")
		}
		if !strings.HasPrefix(a.URL, "https://") && !strings.HasPrefix(a.URL, "s3://") {
			return errors.New("artifact.url must be an https:// or s3:// URL")
		}
		if !sha256Pattern.MatchString(a.Checksum) {
			return errors.New(`artifact.checksum must be "sha256:" followed by 64 lowercase hex characters`)
		}
		if a.SizeBytes <= 0 {
			return errors.New("artifact.size_bytes must be positive")
		}
		if a.Format == "" {
			a.Format = "safetensors"
		}
		switch a.Format {
		case "safetensors", "gguf", "gptq", "awq":
		default:
			return errors.New("artifact.format must be one of safetensors, gguf, gptq, awq")
		}
	}

	return nil
}

// HandleListModels returns the public catalogue plus the caller's own BYO
// models. Previously it returned every row in the table, so one customer's
// private model name and pricing were visible to every other customer.
func (h *ModelHandler) HandleListModels(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.GetClaims(r.Context())
	if !ok || claims == nil {
		writeError(w, http.StatusUnauthorized, "Unauthenticated")
		return
	}

	ctx := r.Context()
	queryValues := r.URL.Query()

	query := modelSelect + `
		WHERE (owner_org_id IS NULL OR owner_org_id = $1)`
	args := []interface{}{claims.OrgID}
	argIdx := 2

	if tier := strings.ToLower(strings.TrimSpace(queryValues.Get("tier"))); tier != "" {
		switch tier {
		case domain.TierT1, domain.TierT2, domain.TierT3:
		default:
			writeError(w, http.StatusBadRequest, "tier must be one of t1, t2, t3")
			return
		}
		query += fmt.Sprintf(" AND $%d = ANY(tiers_allowed)", argIdx)
		args = append(args, tier)
		argIdx++
	}

	if raw := queryValues.Get("min_vram_gb"); raw != "" {
		vram, err := strconv.Atoi(raw)
		if err != nil || vram <= 0 {
			// The old code ignored an unparseable filter, silently returning
			// models the caller's hardware cannot run.
			writeError(w, http.StatusBadRequest, "min_vram_gb must be a positive integer")
			return
		}
		query += fmt.Sprintf(" AND min_vram_gb <= $%d", argIdx)
		args = append(args, vram)
		argIdx++
	}

	query += " ORDER BY params_b ASC, name ASC LIMIT 500;"

	rows, err := h.db.Pool.Query(ctx, query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to query models")
		return
	}
	defer rows.Close()

	models := []domain.Model{}
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to scan model row")
			return
		}
		models = append(models, m)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read models")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"models": models,
		"count":  len(models),
	})
}

func (h *ModelHandler) HandleGetModel(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.GetClaims(r.Context())
	if !ok || claims == nil {
		writeError(w, http.StatusUnauthorized, "Unauthenticated")
		return
	}

	modelID := strings.TrimSpace(r.PathValue("id"))
	if modelID == "" {
		writeError(w, http.StatusBadRequest, "Model ID required")
		return
	}

	ctx := r.Context()

	// A model is addressable by UUID or by name. Both are scoped to the
	// catalogue plus the caller's own models, so a UUID belonging to another
	// tenant is a 404 rather than a disclosure.
	query := modelSelect + `
		WHERE (owner_org_id IS NULL OR owner_org_id = $2)
		  AND (name = $1`
	if _, err := uuid.Parse(modelID); err == nil {
		query += ` OR id = $1::uuid`
	}
	query += `)
		ORDER BY owner_org_id NULLS LAST
		LIMIT 1;`

	rows, err := h.db.Pool.Query(ctx, query, modelID, claims.OrgID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Database error")
		return
	}
	m, err := scanOneModel(rows)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "Model not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "Database error")
		return
	}

	// Only verified artifacts are reported. An unverified row describes bytes
	// nobody has hashed, and a caller must not mistake it for something
	// deployable.
	artQuery := `
		SELECT id, model_id, version, artifact_url, size_bytes, checksum, format, created_at
		FROM model_artifacts
		WHERE model_id = $1 AND verified
		ORDER BY created_at DESC;
	`
	artifacts := []domain.ModelArtifact{}
	artRows, err := h.db.Pool.Query(ctx, artQuery, m.ID)
	if err == nil {
		defer artRows.Close()
		for artRows.Next() {
			var a domain.ModelArtifact
			if err := artRows.Scan(&a.ID, &a.ModelID, &a.Version, &a.ArtifactURL,
				&a.SizeBytes, &a.Checksum, &a.Format, &a.CreatedAt); err != nil {
				writeError(w, http.StatusInternalServerError, "Failed to read artifacts")
				return
			}
			artifacts = append(artifacts, a)
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"model":       m,
		"artifacts":   artifacts,
		"deployable":  len(artifacts) > 0,
	})
}

func (h *ModelHandler) HandleBYOModel(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.GetClaims(r.Context())
	if !ok || claims == nil {
		writeError(w, http.StatusUnauthorized, "Unauthenticated")
		return
	}

	var req BYOModelRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if len(req.TiersAllowed) == 0 {
		// BYO models default to T1 and T2 only per security policy (ADR-007).
		req.TiersAllowed = []string{domain.TierT1, domain.TierT2}
	} else {
		normalized := make([]string, 0, len(req.TiersAllowed))
		for _, t := range req.TiersAllowed {
			switch strings.ToLower(strings.TrimSpace(t)) {
			case domain.TierT1:
				normalized = append(normalized, domain.TierT1)
			case domain.TierT2:
				normalized = append(normalized, domain.TierT2)
			case domain.TierT3:
				// Enforce security policy: BYO weights never land on an
				// unattended personal machine.
				writeError(w, http.StatusForbidden,
					"BYO models are not permitted on Tier 3 (personal) hosts")
				return
			default:
				writeError(w, http.StatusBadRequest, "tiers_allowed must contain only t1 or t2")
				return
			}
		}
		req.TiersAllowed = normalized
	}

	ctx := r.Context()

	tx, err := h.db.Pool.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to begin transaction")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var m domain.Model
	insert := `
		INSERT INTO models (
			name, family, params_b, license, min_vram_gb, tiers_allowed,
			price_in_per_1m, price_out_per_1m, is_byo, owner_org_id, quantization_presets
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, TRUE, $9, '[]'::jsonb)
		RETURNING id, name, family, params_b, license, min_vram_gb, tiers_allowed,
		          price_in_per_1m, price_out_per_1m, price_per_hour_inr, is_byo,
		          quantization_presets, created_at, updated_at;
	`
	err = tx.QueryRow(ctx, insert,
		req.Name, req.Family, req.ParamsB, req.License, req.MinVramGB, req.TiersAllowed,
		req.PriceInPer1M, req.PriceOutPer1M, claims.OrgID,
	).Scan(
		&m.ID, &m.Name, &m.Family, &m.ParamsB, &m.License, &m.MinVramGB, &m.TiersAllowed,
		&m.PriceInPer1M, &m.PriceOutPer1M, &m.PricePerHourINR, &m.IsBYO,
		&m.QuantizationPresets, &m.CreatedAt, &m.UpdatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "A model with that name already exists in this organization")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to register model")
		return
	}

	// The artifact is recorded unverified. Ingest downloads the bytes, hashes
	// them, and only then sets verified — so a caller cannot self-certify an
	// artifact by asserting a digest.
	if req.Artifact != nil {
		artInsert := `
			INSERT INTO model_artifacts (model_id, version, artifact_url, size_bytes, checksum, format, verified)
			VALUES ($1, $2, $3, $4, $5, $6, FALSE);
		`
		if _, err := tx.Exec(ctx, artInsert, m.ID, req.Artifact.Version, req.Artifact.URL,
			req.Artifact.SizeBytes, req.Artifact.Checksum, req.Artifact.Format); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to record model artifact")
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to commit model registration")
		return
	}

	response := map[string]interface{}{"model": m}
	if req.Artifact != nil {
		response["artifact_status"] = "pending_verification"
		response["message"] = "Artifact registered. It will be downloaded and its digest checked before any deployment can use it."
	} else {
		response["artifact_status"] = "none"
		response["message"] = "Model registered. Add a verified artifact before deploying it."
	}
	writeJSON(w, http.StatusCreated, response)
}

// scanModel reads one model row in the modelSelect column order.
func scanModel(rows pgx.Rows) (domain.Model, error) {
	var m domain.Model
	err := rows.Scan(
		&m.ID, &m.Name, &m.Family, &m.ParamsB, &m.License, &m.MinVramGB, &m.TiersAllowed,
		&m.PriceInPer1M, &m.PriceOutPer1M, &m.PricePerHourINR, &m.IsBYO,
		&m.QuantizationPresets, &m.CreatedAt, &m.UpdatedAt,
	)
	return m, err
}

// scanOneModel consumes a single-row result set, returning pgx.ErrNoRows when
// the set is empty.
func scanOneModel(rows pgx.Rows) (domain.Model, error) {
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return domain.Model{}, err
		}
		return domain.Model{}, pgx.ErrNoRows
	}
	m, err := scanModel(rows)
	if err != nil {
		return domain.Model{}, err
	}
	return m, rows.Err()
}
