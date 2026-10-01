package billing

import (
	"context"
	"fmt"

	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/domain"
	"github.com/ayeus/ayeusann/internal/money"
	"github.com/jackc/pgx/v5"
)

// Usage statuses mirror the usage_events.status check constraint.
const (
	StatusSuccess   = "success"
	StatusError     = "error"
	StatusTimeout   = "timeout"
	StatusCancelled = "cancelled"
)

// UsageInput is one completed (or failed) inference request.
type UsageInput struct {
	RequestID    string
	OrgID        string
	DeploymentID string
	ReplicaID    string
	HostID       string
	ModelID      string
	Tier         string
	GPUModel     string
	InputTokens  int
	OutputTokens int
	// GPUSeconds is wall-clock time the request held the replica's GPU. With one
	// job per GPU (ADR-008) this is the honest measure of occupancy.
	GPUSeconds float64
	DurationMs int
	Status     string
}

// UsageResult reports what was recorded.
type UsageResult struct {
	Duplicate    bool
	Charge       money.Amount
	HostAmount   money.Amount
	BalanceAfter *money.Amount
	Prices       Prices
}

// RecordUsage writes the usage event and the wallet debit in one transaction.
//
// Successful and client-cancelled requests are charged for their tokens. Failed
// requests are still recorded (at zero) because the trust engine derives each host's success rate from them.
//
// The debit is unbounded: the request has already consumed GPU time, so
// refusing to record it would only lose revenue. Admission control (a positive
// spendable balance) happens before a request is served.
func RecordUsage(ctx context.Context, client *db.Client, in UsageInput) (*UsageResult, error) {
	if in.RequestID == "" || in.OrgID == "" || in.DeploymentID == "" || in.ReplicaID == "" || in.HostID == "" {
		return nil, fmt.Errorf("billing: usage is missing required identifiers")
	}
	if in.Status == "" {
		in.Status = StatusSuccess
	}

	var res UsageResult
	err := client.ExecTx(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM usage_events WHERE request_id = $1);`, in.RequestID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("billing: duplicate check failed: %w", err)
		}
		if exists {
			res.Duplicate = true
			return nil
		}

		prices, err := ResolvePrices(ctx, tx, in.OrgID, in.ModelID, in.Tier, in.GPUModel)
		if err != nil {
			return err
		}
		res.Prices = prices

		chargeUSD := money.Zero(PriceCurrency)
		// Cancelled requests are charged for the tokens generated before the
		// client disconnected; the GPU time was spent either way.
		if in.Status == StatusSuccess || in.Status == StatusCancelled {
			chargeUSD, err = TokenCharge(int64(in.InputTokens), int64(in.OutputTokens), prices)
			if err != nil {
				return err
			}
		}

		currency, err := db.WalletCurrency(ctx, tx, in.OrgID)
		if err != nil {
			return err
		}
		rate, rateStr, err := FXRate(ctx, tx, PriceCurrency, currency)
		if err != nil {
			return err
		}
		charge, err := Convert(chargeUSD, rate, currency)
		if err != nil {
			return err
		}
		host, _, err := HostShare(charge)
		if err != nil {
			return err
		}
		res.Charge, res.HostAmount = charge, host

		// ON CONFLICT without a target: usage_events is partitioned, and request_id
		// uniqueness is enforced by per-partition unique indexes, which a named
		// conflict target on the parent cannot reference.
		tag, err := tx.Exec(ctx, `
			INSERT INTO usage_events (
				request_id, deployment_id, replica_id, host_id, org_id, model_id,
				input_tokens, output_tokens, gpu_seconds, tier,
				amount_customer, amount_host, currency, fx_rate, duration_ms, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14::NUMERIC, $15, $16)
			ON CONFLICT DO NOTHING;
		`, in.RequestID, in.DeploymentID, in.ReplicaID, in.HostID, in.OrgID, in.ModelID,
			in.InputTokens, in.OutputTokens, in.GPUSeconds, in.Tier,
			charge, host, currency, rateStr, in.DurationMs, in.Status)
		if err != nil {
			return fmt.Errorf("billing: failed to insert usage event: %w", err)
		}
		if tag.RowsAffected() == 0 {
			res.Duplicate = true
			return nil
		}

		failed := 0
		if in.Status != StatusSuccess {
			failed = 1
		}
		if _, err := tx.Exec(ctx, `
			UPDATE replicas
			SET total_requests = total_requests + 1,
			    failed_requests = failed_requests + $2
			WHERE id = $1;
		`, in.ReplicaID, failed); err != nil {
			return fmt.Errorf("billing: failed to update replica counters: %w", err)
		}

		if charge.IsPositive() {
			desc := fmt.Sprintf("Inference: %d in + %d out tokens", in.InputTokens, in.OutputTokens)
			ref := in.RequestID
			entry, err := db.RecordTransactionTx(ctx, tx, db.TransactionRequest{
				OrgID:       in.OrgID,
				Delta:       charge.Neg(),
				Kind:        domain.LedgerKindDebit,
				RefID:       &ref,
				Description: &desc,
				Unbounded:   true,
			})
			if err != nil {
				return err
			}
			res.BalanceAfter = &entry.BalanceAfter
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &res, nil
}
