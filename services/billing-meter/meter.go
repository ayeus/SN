package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/domain"
	"github.com/ayeus/ayeusann/internal/money"
)

var (
	ErrMissingFields    = errors.New("meter: required fields missing")
	ErrModelNotFound    = errors.New("meter: model not found for pricing")
)

const (
	// HostRevenueSharePct is the percentage of customer charges that accrue to the host.
	HostRevenueSharePct = 0.75 // 75% to host, 25% platform fee
	// GSTRate is the GST rate for Indian organizations.
	GSTRate = 0.18
)

// UsageIngestRequest is sent by the inference gateway after each completed request.
type UsageIngestRequest struct {
	RequestID    string  `json:"request_id"`
	DeploymentID string  `json:"deployment_id"`
	ReplicaID    string  `json:"replica_id"`
	HostID       string  `json:"host_id"`
	ModelName    string  `json:"model_name"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	GpuSeconds   float64 `json:"gpu_seconds"`
	Tier         string  `json:"tier"`
	Status       string  `json:"status"` // success|error|timeout|cancelled
}

// UsageSummaryEntry represents aggregated usage for one day.
type UsageSummaryEntry struct {
	Date           string  `json:"date"`
	RequestCount   int     `json:"request_count"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	TotalCost      float64 `json:"total_cost"`
	GpuSecondsUsed float64 `json:"gpu_seconds_used"`
}

// InvoiceGenerateRequest specifies the billing period for invoice generation.
type InvoiceGenerateRequest struct {
	OrgID       string `json:"org_id"`
	PeriodStart string `json:"period_start"` // YYYY-MM-DD
	PeriodEnd   string `json:"period_end"`   // YYYY-MM-DD
}

// MeterService manages usage ingestion, pricing, and billing.
type MeterService struct {
	db     *db.Client
	ledger *db.LedgerService
}

// NewMeterService creates a new MeterService.
func NewMeterService(database *db.Client) *MeterService {
	return &MeterService{
		db:     database,
		ledger: db.NewLedgerService(database),
	}
}

// IngestUsage records a usage event and debits the customer wallet.
// Idempotent by request_id — duplicate submissions are silently ignored.
func (m *MeterService) IngestUsage(ctx context.Context, req UsageIngestRequest) (*domain.UsageEvent, error) {
	if req.RequestID == "" || req.DeploymentID == "" || req.ReplicaID == "" || req.HostID == "" {
		return nil, ErrMissingFields
	}

	if req.Status == "" {
		req.Status = "success"
	}

	// Look up model pricing from the deployment
	var priceInPer1M, priceOutPer1M float64
	var orgID string
	pricingQuery := `
		SELECT m.price_in_per_1m, m.price_out_per_1m, d.org_id
		FROM deployments d
		JOIN models m ON m.id = d.model_id
		WHERE d.id = $1 AND d.deleted_at IS NULL;
	`
	err := m.db.Pool.QueryRow(ctx, pricingQuery, req.DeploymentID).Scan(
		&priceInPer1M, &priceOutPer1M, &orgID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrModelNotFound
		}
		return nil, fmt.Errorf("meter: failed to look up pricing: %w", err)
	}

	// Compute charges
	amountCustomer := computeTokenCost(req.InputTokens, req.OutputTokens, priceInPer1M, priceOutPer1M)
	amountHost := amountCustomer * HostRevenueSharePct

	// Insert usage event (idempotent via ON CONFLICT)
	insertQuery := `
		INSERT INTO usage_events (request_id, deployment_id, replica_id, host_id,
			input_tokens, output_tokens, gpu_seconds, tier,
			amount_customer, amount_host, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (request_id) DO NOTHING
		RETURNING request_id, deployment_id, replica_id, host_id,
			input_tokens, output_tokens, gpu_seconds, tier,
			amount_customer, amount_host, ts, status;
	`

	var event domain.UsageEvent
	err = m.db.Pool.QueryRow(ctx, insertQuery,
		req.RequestID, req.DeploymentID, req.ReplicaID, req.HostID,
		req.InputTokens, req.OutputTokens, req.GpuSeconds, req.Tier,
		amountCustomer, amountHost, req.Status,
	).Scan(
		&event.RequestID, &event.DeploymentID, &event.ReplicaID, &event.HostID,
		&event.InputTokens, &event.OutputTokens, &event.GpuSeconds, &event.Tier,
		&event.AmountCustomer, &event.AmountHost, &event.Timestamp, &event.Status,
	)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Duplicate request_id — idempotent, return success with empty event
			return &domain.UsageEvent{RequestID: req.RequestID, Status: "duplicate"}, nil
		}
		return nil, fmt.Errorf("meter: failed to insert usage event: %w", err)
	}

	// Debit customer wallet
	if amountCustomer > 0 && req.Status == "success" {
		desc := fmt.Sprintf("Inference: %d input + %d output tokens on %s",
			req.InputTokens, req.OutputTokens, req.DeploymentID[:8])
		delta, _ := money.FromFloat(-amountCustomer, "USD")
		_, walletErr := m.ledger.RecordTransaction(ctx, db.TransactionRequest{
			OrgID:       orgID,
			Delta:       delta,
			Kind:        domain.LedgerKindDebit,
			RefID:       &req.RequestID,
			Description: &desc,
		})
		if walletErr != nil {
			// Log but don't fail — usage is already recorded, billing will reconcile
			fmt.Printf("meter: WARNING wallet debit failed for org %s: %v\n", orgID, walletErr)
		}
	}

	return &event, nil
}

// GetUsageSummary returns aggregated daily usage for an organization.
func (m *MeterService) GetUsageSummary(ctx context.Context, orgID string, deploymentID *string, startDate, endDate string) ([]UsageSummaryEntry, error) {
	query := `
		SELECT DATE(ue.ts) as day,
		       COUNT(*) as request_count,
		       SUM(ue.input_tokens) as input_tokens,
		       SUM(ue.output_tokens) as output_tokens,
		       SUM(ue.amount_customer) as total_cost,
		       SUM(ue.gpu_seconds) as gpu_seconds
		FROM usage_events ue
		JOIN deployments d ON d.id = ue.deployment_id
		WHERE d.org_id = $1
		  AND ue.ts >= $2::timestamptz
		  AND ue.ts < $3::timestamptz
		  AND ue.status = 'success'
	`
	args := []interface{}{orgID, startDate, endDate}

	if deploymentID != nil && *deploymentID != "" {
		query += " AND ue.deployment_id = $4"
		args = append(args, *deploymentID)
	}

	query += " GROUP BY DATE(ue.ts) ORDER BY day DESC;"

	rows, err := m.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("meter: usage summary query failed: %w", err)
	}
	defer rows.Close()

	var entries []UsageSummaryEntry
	for rows.Next() {
		var e UsageSummaryEntry
		var day time.Time
		if err := rows.Scan(&day, &e.RequestCount, &e.InputTokens, &e.OutputTokens, &e.TotalCost, &e.GpuSecondsUsed); err != nil {
			return nil, fmt.Errorf("meter: failed to scan usage row: %w", err)
		}
		e.Date = day.Format("2006-01-02")
		entries = append(entries, e)
	}

	return entries, rows.Err()
}

// GetBalance returns the current wallet balance for an organization.
func (m *MeterService) GetBalance(ctx context.Context, orgID string) (money.Amount, error) {
	return m.ledger.GetBalance(ctx, orgID)
}

// GenerateInvoice creates an invoice for the specified billing period.
func (m *MeterService) GenerateInvoice(ctx context.Context, req InvoiceGenerateRequest) (*domain.Invoice, error) {
	if req.OrgID == "" || req.PeriodStart == "" || req.PeriodEnd == "" {
		return nil, ErrMissingFields
	}

	// Aggregate usage for the period
	aggregateQuery := `
		SELECT COALESCE(SUM(ue.amount_customer), 0)
		FROM usage_events ue
		JOIN deployments d ON d.id = ue.deployment_id
		WHERE d.org_id = $1
		  AND ue.ts >= $2::timestamptz
		  AND ue.ts < $3::timestamptz
		  AND ue.status = 'success';
	`

	var subtotal float64
	err := m.db.Pool.QueryRow(ctx, aggregateQuery, req.OrgID, req.PeriodStart, req.PeriodEnd).Scan(&subtotal)
	if err != nil {
		return nil, fmt.Errorf("meter: failed to aggregate usage: %w", err)
	}

	gstAmount := subtotal * GSTRate
	total := subtotal + gstAmount

	// Generate invoice number: INV-{YYYYMM}-{orgID[:8]}
	periodStart, _ := time.Parse("2006-01-02", req.PeriodStart)
	invoiceNumber := fmt.Sprintf("INV-%s-%s", periodStart.Format("200601"), req.OrgID[:8])

	var invoice domain.Invoice
	err = m.db.ExecTx(ctx, func(tx pgx.Tx) error {
		invoiceQuery := `
			INSERT INTO invoices (org_id, number, period_start, period_end, subtotal, tax_amount, tax_name, tax_rate, total, currency, status)
			VALUES ($1, $2, $3, $4, $5, $6, 'GST', '0.18', $7, 'USD', 'issued')
			RETURNING id, org_id, number, period_start, period_end, subtotal, tax_amount, total, status, created_at;
		`
		err := tx.QueryRow(ctx, invoiceQuery,
			req.OrgID, invoiceNumber, req.PeriodStart, req.PeriodEnd,
			subtotal, gstAmount, total,
		).Scan(
			&invoice.ID, &invoice.OrgID, &invoice.Number,
			&invoice.PeriodStart, &invoice.PeriodEnd,
			&invoice.Subtotal, &invoice.TaxAmount, &invoice.Total,
			&invoice.Status, &invoice.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("meter: failed to create invoice: %w", err)
		}

		// Create per-deployment invoice lines
		linesQuery := `
			INSERT INTO invoice_lines (invoice_id, description, quantity, unit_price, amount)
			SELECT $1,
			       'Inference usage: ' || m.name || ' (' || d.name || ')',
			       SUM(ue.input_tokens + ue.output_tokens),
			       CASE WHEN SUM(ue.input_tokens + ue.output_tokens) > 0
			            THEN SUM(ue.amount_customer) / SUM(ue.input_tokens + ue.output_tokens)
			            ELSE 0 END,
			       SUM(ue.amount_customer)
			FROM usage_events ue
			JOIN deployments d ON d.id = ue.deployment_id
			JOIN models m ON m.id = d.model_id
			WHERE d.org_id = $2
			  AND ue.ts >= $3::timestamptz
			  AND ue.ts < $4::timestamptz
			  AND ue.status = 'success'
			GROUP BY d.id, d.name, m.name;
		`
		_, err = tx.Exec(ctx, linesQuery, invoice.ID, req.OrgID, req.PeriodStart, req.PeriodEnd)
		if err != nil {
			return fmt.Errorf("meter: failed to create invoice lines: %w", err)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return &invoice, nil
}

// ListInvoices returns all invoices for an organization.
func (m *MeterService) ListInvoices(ctx context.Context, orgID string) ([]domain.Invoice, error) {
	query := `
		SELECT id, org_id, number, period_start, period_end,
		       subtotal, tax_amount, total, status, pdf_url, created_at
		FROM invoices
		WHERE org_id = $1
		ORDER BY period_start DESC;
	`

	rows, err := m.db.Pool.Query(ctx, query, orgID)
	if err != nil {
		return nil, fmt.Errorf("meter: failed to list invoices: %w", err)
	}
	defer rows.Close()

	var invoices []domain.Invoice
	for rows.Next() {
		var inv domain.Invoice
		if err := rows.Scan(
			&inv.ID, &inv.OrgID, &inv.Number, &inv.PeriodStart, &inv.PeriodEnd,
			&inv.Subtotal, &inv.TaxAmount, &inv.Total, &inv.Status, &inv.PdfURL, &inv.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("meter: failed to scan invoice: %w", err)
		}
		invoices = append(invoices, inv)
	}

	return invoices, rows.Err()
}

// computeTokenCost calculates the customer charge based on token counts and pricing.
func computeTokenCost(inputTokens, outputTokens int, priceInPer1M, priceOutPer1M float64) float64 {
	inputCost := float64(inputTokens) * priceInPer1M / 1_000_000.0
	outputCost := float64(outputTokens) * priceOutPer1M / 1_000_000.0
	return inputCost + outputCost
}

// ─── HTTP Handlers ────────────────────────────────────────────

// RegisterRoutes registers all billing meter HTTP endpoints on the provided mux.
func (m *MeterService) RegisterRoutes(mux *http.ServeMux, svcAuth *auth.ServiceAuthenticator, tm *auth.TokenManager, revStore auth.RevocationStore) {
	authMw := auth.NewMiddleware(tm, revStore)

	// Internal service endpoint: only the inference-gateway may ingest usage
	mux.Handle("POST /v1/usage", svcAuth.RequireInternalService(auth.ServiceInferenceGateway)(http.HandlerFunc(m.handleIngestUsage)))

	// Org-scoped endpoints: protected by JWT Bearer token + matching org_id
	mux.Handle("GET /v1/usage/{org_id}", authMw.Authenticate(auth.RequireOrg("org_id")(http.HandlerFunc(m.handleGetUsageSummary))))
	mux.Handle("GET /v1/balance/{org_id}", authMw.Authenticate(auth.RequireOrg("org_id")(http.HandlerFunc(m.handleGetBalance))))
	mux.Handle("POST /v1/invoices/generate", authMw.Authenticate(http.HandlerFunc(m.handleGenerateInvoice)))
	mux.Handle("GET /v1/invoices/{org_id}", authMw.Authenticate(auth.RequireOrg("org_id")(http.HandlerFunc(m.handleListInvoices))))
}

func (m *MeterService) handleIngestUsage(w http.ResponseWriter, r *http.Request) {
	var req UsageIngestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeMeterError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	event, err := m.IngestUsage(r.Context(), req)
	if err != nil {
		if errors.Is(err, ErrMissingFields) {
			writeMeterError(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, ErrModelNotFound) {
			writeMeterError(w, http.StatusNotFound, err.Error())
			return
		}
		writeMeterError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeMeterJSON(w, http.StatusCreated, event)
}

func (m *MeterService) handleGetUsageSummary(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("org_id")
	if orgID == "" {
		writeMeterError(w, http.StatusBadRequest, "org_id is required")
		return
	}

	startDate := r.URL.Query().Get("start_date")
	endDate := r.URL.Query().Get("end_date")
	if startDate == "" {
		startDate = time.Now().AddDate(0, -1, 0).Format("2006-01-02")
	}
	if endDate == "" {
		endDate = time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	}

	var deploymentID *string
	if did := r.URL.Query().Get("deployment_id"); did != "" {
		deploymentID = &did
	}

	entries, err := m.GetUsageSummary(r.Context(), orgID, deploymentID, startDate, endDate)
	if err != nil {
		writeMeterError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeMeterJSON(w, http.StatusOK, map[string]interface{}{
		"org_id":     orgID,
		"start_date": startDate,
		"end_date":   endDate,
		"entries":    entries,
	})
}

func (m *MeterService) handleGetBalance(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("org_id")
	if orgID == "" {
		writeMeterError(w, http.StatusBadRequest, "org_id is required")
		return
	}

	balance, err := m.GetBalance(r.Context(), orgID)
	if err != nil {
		writeMeterError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeMeterJSON(w, http.StatusOK, map[string]interface{}{
		"org_id":  orgID,
		"balance": balance,
	})
}

func (m *MeterService) handleGenerateInvoice(w http.ResponseWriter, r *http.Request) {
	var req InvoiceGenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeMeterError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if claims, ok := auth.GetClaims(r.Context()); ok && claims != nil {
		if claims.OrgID != "" && req.OrgID != claims.OrgID && !strings.EqualFold(claims.Role, "admin") {
			writeMeterError(w, http.StatusForbidden, "Cannot generate invoice for another organization")
			return
		}
	}

	invoice, err := m.GenerateInvoice(r.Context(), req)
	if err != nil {
		if errors.Is(err, ErrMissingFields) {
			writeMeterError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeMeterError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeMeterJSON(w, http.StatusCreated, invoice)
}

func (m *MeterService) handleListInvoices(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("org_id")
	if orgID == "" {
		writeMeterError(w, http.StatusBadRequest, "org_id is required")
		return
	}

	invoices, err := m.ListInvoices(r.Context(), orgID)
	if err != nil {
		writeMeterError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeMeterJSON(w, http.StatusOK, map[string]interface{}{
		"org_id":   orgID,
		"invoices": invoices,
		"count":    len(invoices),
	})
}

func writeMeterJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeMeterError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
