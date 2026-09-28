package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ayeus/ayeusann/internal/billing"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/domain"
	"github.com/ayeus/ayeusann/internal/httpx"
	"github.com/ayeus/ayeusann/internal/money"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Top-up bounds in INR (PRD F-8: prepaid wallet via UPI, cards, netbanking).
var (
	minTopupINR = money.MustParse("100", "INR")
	maxTopupINR = money.MustParse("500000", "INR")
)

// Razorpay is a minimal client for Orders (top-ups) and webhook verification
// (ADR-006, SRS IR-4). Checkout itself runs in the browser with the order id.
type Razorpay struct {
	keyID, keySecret, webhookSecret string
	baseURL                         string
	client                          *http.Client
}

func (rz *Razorpay) keyIDOrEmpty() string {
	if rz == nil {
		return ""
	}
	return rz.keyID
}

type rzOrder struct {
	ID       string `json:"id"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Status   string `json:"status"`
}

// createOrder creates a Razorpay order. Razorpay amounts are integer paise.
func (rz *Razorpay) createOrder(ctx context.Context, amount money.Amount, receipt string, notes map[string]string) (*rzOrder, error) {
	if amount.Micros()%10_000 != 0 {
		return nil, errors.New("amount must be a whole number of paise")
	}
	body, _ := json.Marshal(map[string]any{
		"amount":   amount.Micros() / 10_000,
		"currency": amount.Currency(),
		"receipt":  receipt,
		"notes":    notes,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rz.baseURL+"/v1/orders", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(rz.keyID, rz.keySecret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := rz.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("razorpay unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("razorpay order failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var o rzOrder
	if err := json.Unmarshal(raw, &o); err != nil || o.ID == "" {
		return nil, fmt.Errorf("razorpay returned an unexpected order: %s", raw)
	}
	return &o, nil
}

// verifyWebhook checks X-Razorpay-Signature: hex(HMAC-SHA256(secret, body)).
func (rz *Razorpay) verifyWebhook(body []byte, signature string) bool {
	mac := hmac.New(sha256.New, []byte(rz.webhookSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(strings.TrimSpace(signature)))
}

type topupRequest struct {
	Amount string `json:"amount"` // decimal, in the wallet currency
	// Method is "razorpay" (default) or "test" (development only).
	Method string `json:"method"`
}

// handleTopup starts a wallet top-up. With Razorpay the wallet is credited by
// the webhook, never by the browser: a client can claim anything, a signed
// webhook cannot be forged.
func (m *Meter) handleTopup(w http.ResponseWriter, r *http.Request) {
	var req topupRequest
	if httpx.DecodeJSON(w, r, &req) != nil {
		return
	}
	ctx := r.Context()
	orgID := claims(r).OrgID

	var currency string
	if err := m.db.Pool.QueryRow(ctx, `SELECT currency FROM organizations WHERE id = $1;`, orgID).Scan(&currency); err != nil {
		httpx.WriteProblem(w, http.StatusNotFound, "Organisation not found")
		return
	}
	amount, err := money.Parse(req.Amount, currency)
	if err != nil || !amount.IsPositive() {
		httpx.WriteProblemFields(w, http.StatusBadRequest, "Enter a positive amount", map[string]string{"amount": "Positive number"})
		return
	}

	if req.Method == "test" {
		if !m.allowTestCredit {
			httpx.WriteProblem(w, http.StatusForbidden, "Test credit is only available in development")
			return
		}
		desc := "Test top-up (development only — not real money)"
		entry, err := m.ledger.RecordTransaction(ctx, db.TransactionRequest{OrgID: orgID, Delta: amount, Kind: domain.LedgerKindTopup, Description: &desc})
		if err != nil {
			httpx.WriteProblem(w, http.StatusInternalServerError, "Failed to credit wallet")
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, map[string]any{"method": "test", "entry": entry})
		return
	}

	if m.razorpay == nil {
		httpx.WriteProblem(w, http.StatusServiceUnavailable, "Online payments are not configured on this installation")
		return
	}
	if currency != "INR" {
		httpx.WriteProblem(w, http.StatusUnprocessableEntity, "Razorpay top-ups are INR only; international card payments are not available yet")
		return
	}
	if amount.LessThan(minTopupINR) || amount.GreaterThan(maxTopupINR) {
		httpx.WriteProblemFields(w, http.StatusBadRequest, "Amount out of range",
			map[string]string{"amount": fmt.Sprintf("Between %s and %s", minTopupINR.Display(), maxTopupINR.Display())})
		return
	}

	rate, rateStr, err := billing.FXRate(ctx, m.db.Pool, currency, billing.PriceCurrency)
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "FX rate unavailable")
		return
	}
	amountUSD, _ := billing.Convert(amount, rate, billing.PriceCurrency)

	intentID := uuid.NewString()
	order, err := m.razorpay.createOrder(ctx, amount, "topup_"+intentID[:30], map[string]string{"org_id": orgID, "intent_id": intentID})
	if err != nil {
		m.log.Error("razorpay order creation failed", "err", err)
		httpx.WriteProblem(w, http.StatusBadGateway, "Payment provider error; please try again")
		return
	}
	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey == "" {
		idemKey = intentID
	}
	if _, err := m.db.Pool.Exec(ctx, `
		INSERT INTO payment_intents (id, org_id, provider, provider_intent_id, amount, currency, amount_usd, fx_rate, status, idempotency_key)
		VALUES ($1, $2, 'razorpay', $3, $4, $5, $6, $7::NUMERIC, 'pending', $8);
	`, intentID, orgID, order.ID, amount, currency, amountUSD, rateStr, orgID+":"+idemKey); err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "Failed to record payment")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"method":    "razorpay",
		"intent_id": intentID,
		"order_id":  order.ID,
		"amount":    order.Amount,
		"currency":  order.Currency,
		"key_id":    m.razorpay.keyID,
	})
}

// handleRazorpayWebhook credits the wallet when an order is paid. Webhooks are
// retried by Razorpay, so both the event and the intent are idempotent.
func (m *Meter) handleRazorpayWebhook(w http.ResponseWriter, r *http.Request) {
	if m.razorpay == nil {
		httpx.WriteProblem(w, http.StatusNotFound, "Not found")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil || !m.razorpay.verifyWebhook(body, r.Header.Get("X-Razorpay-Signature")) {
		httpx.WriteProblem(w, http.StatusUnauthorized, "Invalid webhook signature")
		return
	}

	var evt struct {
		Event   string `json:"event"`
		Payload struct {
			Order struct {
				Entity struct {
					ID         string `json:"id"`
					AmountPaid int64  `json:"amount_paid"`
					Currency   string `json:"currency"`
				} `json:"entity"`
			} `json:"order"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &evt); err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "Invalid payload")
		return
	}
	eventID := r.Header.Get("X-Razorpay-Event-Id")
	if eventID == "" {
		sum := sha256.Sum256(body)
		eventID = hex.EncodeToString(sum[:])
	}
	ctx := r.Context()

	tag, err := m.db.Pool.Exec(ctx, `
		INSERT INTO payment_webhook_events (provider, provider_event_id, event_type, payload)
		VALUES ('razorpay', $1, $2, $3) ON CONFLICT (provider, provider_event_id) DO NOTHING;
	`, eventID, evt.Event, body)
	if err != nil {
		httpx.WriteProblem(w, http.StatusInternalServerError, "Failed to record webhook")
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
		return
	}
	if evt.Event != "order.paid" {
		_, _ = m.db.Pool.Exec(ctx, `UPDATE payment_webhook_events SET processed_at = NOW() WHERE provider = 'razorpay' AND provider_event_id = $1;`, eventID)
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	orderID := evt.Payload.Order.Entity.ID
	err = m.db.ExecTx(ctx, func(tx pgx.Tx) error {
		var intentID, orgID, status string
		var amount money.Amount
		var currency string
		if err := tx.QueryRow(ctx, `
			SELECT id, org_id, status, amount, currency FROM payment_intents
			WHERE provider = 'razorpay' AND provider_intent_id = $1 FOR UPDATE;
		`, orderID).Scan(&intentID, &orgID, &status, &amount, &currency); err != nil {
			return fmt.Errorf("unknown order %s: %w", orderID, err)
		}
		if status == "succeeded" {
			return nil
		}
		amount = money.FromMicros(amount.Micros(), currency)
		paid := money.FromMicros(evt.Payload.Order.Entity.AmountPaid*10_000, currency)
		if paid.Micros() != amount.Micros() {
			return fmt.Errorf("order %s paid %s, expected %s", orderID, paid, amount)
		}
		desc := "Wallet top-up via Razorpay (" + orderID + ")"
		entry, err := db.RecordTransactionTx(ctx, tx, db.TransactionRequest{
			OrgID: orgID, Delta: amount, Kind: domain.LedgerKindTopup, RefID: &intentID, Description: &desc,
		})
		if err != nil && !errors.Is(err, db.ErrDuplicateRef) {
			return err
		}
		var entryID *string
		if entry != nil {
			entryID = &entry.EntryID
		}
		if _, err := tx.Exec(ctx, `
			UPDATE payment_intents SET status = 'succeeded', ledger_entry_id = COALESCE($2, ledger_entry_id), updated_at = NOW()
			WHERE id = $1;
		`, intentID, entryID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE payment_webhook_events SET processed_at = NOW() WHERE provider = 'razorpay' AND provider_event_id = $1;`, eventID)
		return err
	})
	if err != nil {
		m.log.Error("razorpay webhook processing failed", "event_id", eventID, "err", err)
		_, _ = m.db.Pool.Exec(context.WithoutCancel(ctx),
			`UPDATE payment_webhook_events SET error = $2 WHERE provider = 'razorpay' AND provider_event_id = $1;`, eventID, err.Error())
		// 500 makes Razorpay retry; the event row lets a retry be reprocessed.
		_, _ = m.db.Pool.Exec(context.WithoutCancel(ctx),
			`DELETE FROM payment_webhook_events WHERE provider = 'razorpay' AND provider_event_id = $1 AND processed_at IS NULL;`, eventID)
		httpx.WriteProblem(w, http.StatusInternalServerError, "Webhook processing failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "credited"})
}

func newRazorpay(keyID, keySecret, webhookSecret string) *Razorpay {
	if keyID == "" || keySecret == "" || webhookSecret == "" {
		return nil
	}
	return &Razorpay{
		keyID: keyID, keySecret: keySecret, webhookSecret: webhookSecret,
		baseURL: "https://api.razorpay.com",
		client:  &http.Client{Timeout: 15 * time.Second},
	}
}
