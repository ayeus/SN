package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/billing"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/domain"
	"github.com/ayeus/ayeusann/internal/httpx"
	"github.com/ayeus/ayeusann/internal/money"
	"github.com/ayeus/ayeusann/internal/platform"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// ledgerDrift is the Implementation Guide §5 "Money" gate: Σ usage charges must
// equal Σ usage debits in the ledger. Alert on any non-zero value.
var ledgerDrift = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "ayeusann_billing_ledger_drift_micros",
	Help: "Σ usage charges − Σ usage wallet debits over the last 30 days, in micro-units, by currency. Should be 0.",
}, []string{"currency"})

// Meter serves wallet, usage, invoice and top-up endpoints (SRS §2.8).
type Meter struct {
	db       *db.Client
	ledger   *db.LedgerService
	log      *slog.Logger
	razorpay *Razorpay // nil when not configured
	// allowTestCredit enables the development-only wallet top-up.
	allowTestCredit bool
}

func (m *Meter) Routes(tm *auth.TokenManager, rev auth.RevocationStore) http.Handler {
	mux := http.NewServeMux()
	authn := auth.NewMiddleware(tm, rev)
	idem := httpx.NewIdempotency(m.db.Pool, func(r *http.Request) string {
		if c, ok := auth.GetClaims(r.Context()); ok {
			return c.OrgID
		}
		return ""
	})
	org := func(h http.HandlerFunc) http.Handler { return authn.Authenticate(idem.Wrap(h)) }
	billingRole := func(h http.HandlerFunc) http.Handler {
		return authn.Authenticate(idem.Wrap(auth.RequireRole("admin", "billing")(h)))
	}

	mux.Handle("GET /v1/billing/wallet", org(m.handleWallet))
	mux.Handle("GET /v1/billing/ledger", org(m.handleLedger))
	mux.Handle("POST /v1/billing/topup", billingRole(m.handleTopup))
	mux.HandleFunc("POST /v1/billing/webhooks/razorpay", m.handleRazorpayWebhook)
	mux.Handle("GET /v1/usage", org(m.handleUsage))
	mux.Handle("GET /v1/invoices", org(m.handleListInvoices))
	mux.Handle("GET /v1/invoices/{id}", org(m.handleGetInvoice))
	mux.Handle("POST /v1/invoices/generate", billingRole(m.handleGenerateInvoice))
	return platform.MetricsMiddleware("billing-meter", mux)
}

func claims(r *http.Request) *auth.Claims {
	c, _ := auth.GetClaims(r.Context())
	return c
}

// ─── Wallet ───────────────────────────────────────────────────

func (m *Meter) handleWallet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := claims(r).OrgID
	balance, err := m.ledger.GetBalance(ctx, orgID)
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "Failed to read balance")
		return
	}
	cur := balance.Currency()

	var creditLimit, lowThreshold money.Amount
	_ = m.db.Pool.QueryRow(ctx, `SELECT credit_limit, low_balance_threshold FROM wallet_settings WHERE org_id = $1;`, orgID).
		Scan(&creditLimit, &lowThreshold)

	_, fx, _ := billing.FXRate(ctx, m.db.Pool, billing.PriceCurrency, cur)

	var spend24h, spend30d money.Amount
	_ = m.db.Pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_customer) FILTER (WHERE ts >= NOW() - INTERVAL '24 hours'), 0),
		       COALESCE(SUM(amount_customer), 0)
		FROM usage_events WHERE org_id = $1 AND ts >= NOW() - INTERVAL '30 days';
	`, orgID).Scan(&spend24h, &spend30d)

	low := balance.LessThan(money.FromMicros(lowThreshold.Micros(), cur))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"balance":               balance,
		"currency":              cur,
		"credit_limit":          money.FromMicros(creditLimit.Micros(), cur),
		"low_balance_threshold": money.FromMicros(lowThreshold.Micros(), cur),
		"low_balance":           low,
		"spend_24h":             money.FromMicros(spend24h.Micros(), cur),
		"spend_30d":             money.FromMicros(spend30d.Micros(), cur),
		"fx_from_usd":           fx,
		"topup": map[string]any{
			"razorpay":    m.razorpay != nil,
			"test_credit": m.allowTestCredit,
			"key_id":      m.razorpay.keyIDOrEmpty(),
		},
	})
}

func (m *Meter) handleLedger(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := m.db.Pool.Query(r.Context(), `
		SELECT entry_id, org_id, delta, balance_after, kind, ref_id, description, currency, created_at
		FROM wallet_ledger WHERE org_id = $1 ORDER BY seq DESC LIMIT $2;
	`, claims(r).OrgID, limit)
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "Failed to read ledger")
		return
	}
	defer rows.Close()
	entries := []domain.WalletLedger{}
	for rows.Next() {
		var e domain.WalletLedger
		if err := rows.Scan(&e.EntryID, &e.OrgID, &e.Delta, &e.BalanceAfter, &e.Kind, &e.RefID, &e.Description, &e.Currency, &e.CreatedAt); err == nil {
			e.Delta = money.FromMicros(e.Delta.Micros(), e.Currency)
			e.BalanceAfter = money.FromMicros(e.BalanceAfter.Micros(), e.Currency)
			entries = append(entries, e)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// ─── Usage (GET /v1/usage?from&to&group_by) ───────────────────

func (m *Meter) handleUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to, err := parseRange(q.Get("from"), q.Get("to"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, err.Error())
		return
	}
	var keyExpr, label string
	switch q.Get("group_by") {
	case "", "day":
		keyExpr, label = "TO_CHAR(DATE_TRUNC('day', u.ts), 'YYYY-MM-DD')", "day"
	case "deployment":
		keyExpr, label = "d.name", "deployment"
	case "model":
		keyExpr, label = "m.name", "model"
	default:
		httpx.WriteProblem(w, http.StatusBadRequest, "group_by must be day, deployment or model")
		return
	}
	orgID := claims(r).OrgID
	rows, err := m.db.Pool.Query(r.Context(), `
		SELECT `+keyExpr+` AS k, COUNT(*), COUNT(*) FILTER (WHERE u.status <> 'success'),
		       COALESCE(SUM(u.input_tokens), 0), COALESCE(SUM(u.output_tokens), 0),
		       COALESCE(SUM(u.gpu_seconds), 0)::FLOAT8, COALESCE(SUM(u.amount_customer), 0), MAX(u.currency)
		FROM usage_events u
		JOIN deployments d ON d.id = u.deployment_id
		JOIN models m ON m.id = d.model_id
		WHERE u.org_id = $1 AND u.ts >= $2 AND u.ts < $3
		GROUP BY k ORDER BY k;
	`, orgID, from, to)
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "Failed to read usage")
		return
	}
	defer rows.Close()
	type row struct {
		Key          string       `json:"key"`
		Requests     int64        `json:"requests"`
		Errors       int64        `json:"errors"`
		InputTokens  int64        `json:"input_tokens"`
		OutputTokens int64        `json:"output_tokens"`
		GPUSeconds   float64      `json:"gpu_seconds"`
		Cost         money.Amount `json:"cost"`
	}
	out := []row{}
	for rows.Next() {
		var x row
		var cost money.Amount
		var cur *string
		if err := rows.Scan(&x.Key, &x.Requests, &x.Errors, &x.InputTokens, &x.OutputTokens, &x.GPUSeconds, &cost, &cur); err != nil {
			continue
		}
		c := "USD"
		if cur != nil {
			c = *cur
		}
		x.Cost = money.FromMicros(cost.Micros(), c)
		out = append(out, x)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"group_by": label, "from": from, "to": to, "rows": out})
}

func parseRange(fromS, toS string) (time.Time, time.Time, error) {
	to := time.Now().UTC().Add(time.Minute)
	from := to.AddDate(0, 0, -30)
	var err error
	if fromS != "" {
		if from, err = parseDate(fromS); err != nil {
			return from, to, fmt.Errorf("from: %w", err)
		}
	}
	if toS != "" {
		if to, err = parseDate(toS); err != nil {
			return from, to, fmt.Errorf("to: %w", err)
		}
	}
	if !from.Before(to) {
		return from, to, errors.New("from must be before to")
	}
	return from, to, nil
}

func parseDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}

// ─── Reconciliation ───────────────────────────────────────────

// Reconcile compares usage charges with the wallet debits that paid for them
// and publishes the difference per currency.
func (m *Meter) Reconcile(ctx context.Context) error {
	rows, err := m.db.Pool.Query(ctx, `
		WITH charges AS (
			SELECT currency, SUM(amount_customer) AS amt FROM usage_events
			WHERE ts >= NOW() - INTERVAL '30 days' AND amount_customer > 0 GROUP BY currency
		), debits AS (
			SELECT l.currency, -SUM(l.delta) AS amt
			FROM wallet_ledger l
			WHERE l.kind = 'debit' AND l.created_at >= NOW() - INTERVAL '30 days'
			  AND EXISTS (SELECT 1 FROM usage_events u WHERE u.request_id = l.ref_id AND u.ts >= NOW() - INTERVAL '31 days')
			GROUP BY l.currency
		)
		SELECT COALESCE(c.currency, d.currency), COALESCE(c.amt, 0) - COALESCE(d.amt, 0)
		FROM charges c FULL OUTER JOIN debits d ON d.currency = c.currency;
	`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cur string
		var drift money.Amount
		if err := rows.Scan(&cur, &drift); err != nil {
			return err
		}
		ledgerDrift.WithLabelValues(cur).Set(float64(drift.Micros()))
		if !drift.IsZero() {
			m.log.Error("ledger drift detected", "currency", cur, "drift", drift.String())
		}
	}
	return rows.Err()
}

// ─── Invoices (SRS FR-73) ─────────────────────────────────────

type invoiceRequest struct {
	PeriodStart string `json:"period_start"` // YYYY-MM-DD, inclusive
	PeriodEnd   string `json:"period_end"`   // YYYY-MM-DD, exclusive
}

func (m *Meter) handleGenerateInvoice(w http.ResponseWriter, r *http.Request) {
	var req invoiceRequest
	if httpx.DecodeJSON(w, r, &req) != nil {
		return
	}
	start, err1 := time.Parse("2006-01-02", req.PeriodStart)
	end, err2 := time.Parse("2006-01-02", req.PeriodEnd)
	if err1 != nil || err2 != nil || !start.Before(end) {
		httpx.WriteProblem(w, http.StatusBadRequest, "period_start and period_end must be YYYY-MM-DD with start before end")
		return
	}
	inv, err := m.GenerateInvoice(r.Context(), claims(r).OrgID, start, end)
	if err != nil {
		m.log.Error("invoice generation failed", "err", err)
		httpx.WriteProblem(w, http.StatusInternalServerError, "Failed to generate invoice")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"invoice": inv})
}

// GenerateInvoice builds an invoice for a period from usage events. Amounts are
// already in the wallet currency; tax comes from the organisation's billing
// country via tax_jurisdictions (GST 18% for India). Re-running for the same
// period returns the existing invoice.
func (m *Meter) GenerateInvoice(ctx context.Context, orgID string, start, end time.Time) (*domain.Invoice, error) {
	var inv domain.Invoice
	err := m.db.ExecTx(ctx, func(tx pgx.Tx) error {
		var currency string
		var country *string
		if err := tx.QueryRow(ctx, `SELECT currency, billing_country FROM organizations WHERE id = $1;`, orgID).Scan(&currency, &country); err != nil {
			return err
		}
		number := fmt.Sprintf("INV-%s-%s-%s", start.Format("20060102"), end.Format("20060102"), orgID[:8])

		if err := scanInvoice(tx.QueryRow(ctx, invoiceSelect+` WHERE number = $1 AND org_id = $2;`, number, orgID), &inv); err == nil {
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var subtotal money.Amount
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(amount_customer), 0) FROM usage_events
			WHERE org_id = $1 AND ts >= $2 AND ts < $3 AND status = 'success';
		`, orgID, start, end).Scan(&subtotal); err != nil {
			return err
		}
		subtotal = money.FromMicros(subtotal.Micros(), currency)

		taxName, rateStr := "Tax", "0"
		if country != nil {
			_ = tx.QueryRow(ctx, `
				SELECT tax_name, rate::TEXT FROM tax_jurisdictions
				WHERE country_code = $1 AND region_code IS NULL AND effective_from <= $2
				  AND (effective_to IS NULL OR effective_to > $2)
				ORDER BY effective_from DESC LIMIT 1;
			`, *country, start).Scan(&taxName, &rateStr)
		}
		num, den, err := decimalRatio(rateStr)
		if err != nil {
			return err
		}
		tax, err := subtotal.MulRate(num, den)
		if err != nil {
			return err
		}
		total, err := subtotal.Add(tax)
		if err != nil {
			return err
		}

		if err := scanInvoice(tx.QueryRow(ctx, `
			INSERT INTO invoices (org_id, number, period_start, period_end, subtotal, tax_amount, tax_name, tax_rate,
			                      tax_country, total, currency, fx_rate, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::NUMERIC, $9, $10, $11, 1, 'issued')
			RETURNING `+invoiceColumns+`;
		`, orgID, number, start, end, subtotal, tax, taxName, rateStr, country, total, currency), &inv); err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO invoice_lines (invoice_id, description, quantity, unit_price, amount)
			SELECT $1, 'Inference — ' || d.name || ' (' || m.name || ', ' || UPPER(u.tier) || ')',
			       SUM(u.input_tokens + u.output_tokens),
			       CASE WHEN SUM(u.input_tokens + u.output_tokens) > 0
			            THEN SUM(u.amount_customer) / SUM(u.input_tokens + u.output_tokens) ELSE 0 END,
			       SUM(u.amount_customer)
			FROM usage_events u JOIN deployments d ON d.id = u.deployment_id JOIN models m ON m.id = d.model_id
			WHERE u.org_id = $2 AND u.ts >= $3 AND u.ts < $4 AND u.status = 'success'
			GROUP BY d.name, m.name, u.tier;
		`, inv.ID, orgID, start, end)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &inv, m.loadLines(ctx, &inv)
}

// decimalRatio turns "0.1800" into 1800/10000 for exact multiplication.
func decimalRatio(s string) (int64, int64, error) {
	a, err := money.Parse(s, "")
	if err != nil {
		return 0, 0, err
	}
	return a.Micros(), 1_000_000, nil
}

const invoiceColumns = `id, org_id, number, period_start, period_end, subtotal, tax_amount, tax_name, tax_rate::TEXT,
	total, currency, fx_rate::TEXT, tax_country, reverse_charge, status, pdf_url, created_at`

const invoiceSelect = `SELECT ` + invoiceColumns + ` FROM invoices`

func scanInvoice(row pgx.Row, inv *domain.Invoice) error {
	err := row.Scan(&inv.ID, &inv.OrgID, &inv.Number, &inv.PeriodStart, &inv.PeriodEnd, &inv.Subtotal, &inv.TaxAmount,
		&inv.TaxName, &inv.TaxRate, &inv.Total, &inv.Currency, &inv.FxRate, &inv.TaxCountry, &inv.ReverseCharge,
		&inv.Status, &inv.PdfURL, &inv.CreatedAt)
	if err == nil {
		for _, a := range []*money.Amount{&inv.Subtotal, &inv.TaxAmount, &inv.Total} {
			*a = money.FromMicros(a.Micros(), inv.Currency)
		}
	}
	return err
}

func (m *Meter) loadLines(ctx context.Context, inv *domain.Invoice) error {
	rows, err := m.db.Pool.Query(ctx, `
		SELECT id, invoice_id, description, quantity::FLOAT8, unit_price, amount, created_at
		FROM invoice_lines WHERE invoice_id = $1 ORDER BY description;
	`, inv.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	inv.Lines = []domain.InvoiceLine{}
	for rows.Next() {
		var l domain.InvoiceLine
		if err := rows.Scan(&l.ID, &l.InvoiceID, &l.Description, &l.Quantity, &l.UnitPrice, &l.Amount, &l.CreatedAt); err != nil {
			return err
		}
		l.UnitPrice = money.FromMicros(l.UnitPrice.Micros(), inv.Currency)
		l.Amount = money.FromMicros(l.Amount.Micros(), inv.Currency)
		inv.Lines = append(inv.Lines, l)
	}
	return rows.Err()
}

func (m *Meter) handleListInvoices(w http.ResponseWriter, r *http.Request) {
	rows, err := m.db.Pool.Query(r.Context(), invoiceSelect+` WHERE org_id = $1 ORDER BY period_start DESC;`, claims(r).OrgID)
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "Failed to list invoices")
		return
	}
	defer rows.Close()
	list := []domain.Invoice{}
	for rows.Next() {
		var inv domain.Invoice
		if err := scanInvoice(rows, &inv); err == nil {
			list = append(list, inv)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"invoices": list})
}

func (m *Meter) handleGetInvoice(w http.ResponseWriter, r *http.Request) {
	var inv domain.Invoice
	if err := scanInvoice(m.db.Pool.QueryRow(r.Context(), invoiceSelect+` WHERE id::TEXT = $1 AND org_id = $2;`,
		r.PathValue("id"), claims(r).OrgID), &inv); err != nil {
		httpx.WriteProblem(w, http.StatusNotFound, "Invoice not found")
		return
	}
	if err := m.loadLines(r.Context(), &inv); err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "Failed to read invoice lines")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"invoice": inv})
}
