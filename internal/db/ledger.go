package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/ayeus/ayeusann/internal/domain"
	"github.com/ayeus/ayeusann/internal/money"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrInsufficientBalance = errors.New("ledger: insufficient balance for debit")
	ErrInvalidDelta        = errors.New("ledger: delta cannot be zero")
	ErrOrgNotFound         = errors.New("ledger: organization not found")
	ErrDuplicateRef        = errors.New("ledger: a transaction with this reference already exists")
	ErrWrongCurrency       = errors.New("ledger: amount currency does not match the wallet currency")
)

// Querier is the subset of pgx shared by a pool and a transaction, so helpers
// can run either standalone or inside a caller's transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// LedgerService manages wallet transactions and ledger entries.
//
// Invariants:
//   - The ledger is append-only. Corrections are compensating entries.
//   - Every entry records the running balance after it is applied.
//   - Amounts are exact decimals, never floats.
//   - A wallet is denominated in its organisation's billing currency (INR for
//     India per SRS FR-70). Callers convert before calling; the ledger rejects
//     an amount in any other currency instead of silently relabelling it.
//   - Concurrent transactions for one org are serialized by an advisory lock on
//     the org, so two simultaneous debits cannot both read the same prior balance.
type LedgerService struct {
	client *Client
}

// NewLedgerService creates a new LedgerService.
func NewLedgerService(client *Client) *LedgerService {
	return &LedgerService{client: client}
}

// WalletCurrency returns the currency an organisation's wallet is held in.
func WalletCurrency(ctx context.Context, q Querier, orgID string) (string, error) {
	var currency string
	if err := q.QueryRow(ctx, `SELECT currency FROM organizations WHERE id = $1;`, orgID).Scan(&currency); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrOrgNotFound
		}
		return "", fmt.Errorf("ledger: failed to read wallet currency: %w", err)
	}
	return currency, nil
}

// GetBalance returns the current wallet balance for an organization, in the
// organisation's billing currency.
func (s *LedgerService) GetBalance(ctx context.Context, orgID string) (money.Amount, error) {
	var currency string
	if err := s.client.Pool.QueryRow(ctx,
		`SELECT currency FROM organizations WHERE id = $1;`, orgID,
	).Scan(&currency); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return money.Amount{}, ErrOrgNotFound
		}
		return money.Amount{}, fmt.Errorf("ledger: failed to read org: %w", err)
	}

	var balance money.Amount
	err := s.client.Pool.QueryRow(ctx, `
		SELECT balance_after
		FROM wallet_ledger
		WHERE org_id = $1
		ORDER BY seq DESC
		LIMIT 1;
	`, orgID).Scan(&balance)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return money.Zero(currency), nil
		}
		return money.Amount{}, fmt.Errorf("ledger: failed to get balance: %w", err)
	}
	return money.FromMicros(balance.Micros(), currency), nil
}

// TransactionRequest describes a wallet movement.
type TransactionRequest struct {
	OrgID string
	// Delta is positive for credits and negative for debits, in the wallet's
	// currency.
	Delta money.Amount
	Kind  string
	// RefID links the entry to its source (payment intent, usage request).
	// When set, it is enforced unique per kind so retries cannot double-apply.
	RefID       *string
	Description *string
	// AllowOverdraftTo permits the balance to fall to this limit (a non-negative
	// magnitude). Zero means no overdraft.
	AllowOverdraftTo money.Amount
	// Unbounded skips the overdraft floor entirely. It exists for usage that has
	// already been served: refusing to record a completed request does not undo
	// the GPU time, it only loses the revenue. Admission control happens before
	// the request is served, not here.
	Unbounded bool
}

// RecordTransaction atomically records a credit or debit in its own transaction.
func (s *LedgerService) RecordTransaction(ctx context.Context, req TransactionRequest) (*domain.WalletLedger, error) {
	var entry *domain.WalletLedger
	err := s.client.ExecTx(ctx, func(tx pgx.Tx) error {
		var err error
		entry, err = RecordTransactionTx(ctx, tx, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	return entry, nil
}

// RecordTransactionTx records a wallet movement inside the caller's transaction.
// Metering uses this so that the usage event, the customer debit and the host
// accrual commit or roll back together.
//
// The org's ledger is serialized with pg_advisory_xact_lock, taken before the
// balance is read, so concurrent first debits cannot both see a zero balance.
func RecordTransactionTx(ctx context.Context, tx pgx.Tx, req TransactionRequest) (*domain.WalletLedger, error) {
	if req.Delta.IsZero() {
		return nil, ErrInvalidDelta
	}
	if req.OrgID == "" {
		return nil, ErrOrgNotFound
	}
	if req.AllowOverdraftTo.IsNegative() {
		return nil, errors.New("ledger: overdraft limit must be expressed as a non-negative magnitude")
	}

	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0));`, req.OrgID,
	); err != nil {
		return nil, fmt.Errorf("ledger: failed to acquire org lock: %w", err)
	}

	currency, err := WalletCurrency(ctx, tx, req.OrgID)
	if err != nil {
		return nil, err
	}
	if c := req.Delta.Currency(); c != "" && c != currency {
		return nil, fmt.Errorf("%w: got %s, wallet is %s", ErrWrongCurrency, c, currency)
	}

	// Reject a replayed reference before touching the balance.
	if req.RefID != nil && *req.RefID != "" {
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM wallet_ledger
				WHERE org_id = $1 AND ref_id = $2 AND kind = $3
			);
		`, req.OrgID, *req.RefID, req.Kind).Scan(&exists); err != nil {
			return nil, fmt.Errorf("ledger: failed to check for duplicate reference: %w", err)
		}
		if exists {
			return nil, ErrDuplicateRef
		}
	}

	current := money.Zero(currency)
	var prior money.Amount
	err = tx.QueryRow(ctx, `
		SELECT balance_after
		FROM wallet_ledger
		WHERE org_id = $1
		ORDER BY seq DESC
		LIMIT 1;
	`, req.OrgID).Scan(&prior)
	switch {
	case err == nil:
		current = money.FromMicros(prior.Micros(), currency)
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return nil, fmt.Errorf("ledger: failed to read current balance: %w", err)
	}

	delta := money.FromMicros(req.Delta.Micros(), currency)
	newBalance, err := current.Add(delta)
	if err != nil {
		return nil, fmt.Errorf("ledger: balance arithmetic failed: %w", err)
	}

	if !req.Unbounded && delta.IsNegative() {
		floor := money.FromMicros(-req.AllowOverdraftTo.Micros(), currency)
		if newBalance.LessThan(floor) {
			return nil, fmt.Errorf("%w: balance %s plus delta %s would breach the %s floor",
				ErrInsufficientBalance, current, delta, floor)
		}
	}

	var entry domain.WalletLedger
	err = tx.QueryRow(ctx, `
		INSERT INTO wallet_ledger (org_id, delta, balance_after, kind, ref_id, description, currency)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING entry_id, org_id, delta, balance_after, kind, ref_id, description, currency, created_at;
	`, req.OrgID, delta, newBalance, req.Kind, req.RefID, req.Description, currency,
	).Scan(
		&entry.EntryID, &entry.OrgID, &entry.Delta, &entry.BalanceAfter,
		&entry.Kind, &entry.RefID, &entry.Description, &entry.Currency, &entry.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("ledger: failed to insert entry: %w", err)
	}
	entry.Delta = money.FromMicros(entry.Delta.Micros(), entry.Currency)
	entry.BalanceAfter = money.FromMicros(entry.BalanceAfter.Micros(), entry.Currency)
	return &entry, nil
}

// Credit adds funds to an organization's wallet.
func (s *LedgerService) Credit(ctx context.Context, orgID string, amount money.Amount, kind string, refID, description *string) (*domain.WalletLedger, error) {
	if !amount.IsPositive() {
		return nil, errors.New("ledger: credit amount must be positive")
	}
	return s.RecordTransaction(ctx, TransactionRequest{
		OrgID:       orgID,
		Delta:       amount,
		Kind:        kind,
		RefID:       refID,
		Description: description,
	})
}

// Debit removes funds, honouring the org's configured credit limit.
func (s *LedgerService) Debit(ctx context.Context, orgID string, amount money.Amount, kind string, refID, description *string) (*domain.WalletLedger, error) {
	if !amount.IsPositive() {
		return nil, errors.New("ledger: debit amount must be positive")
	}

	creditLimit, err := s.GetCreditLimit(ctx, orgID)
	if err != nil {
		return nil, err
	}

	return s.RecordTransaction(ctx, TransactionRequest{
		OrgID:            orgID,
		Delta:            amount.Neg(),
		Kind:             kind,
		RefID:            refID,
		Description:      description,
		AllowOverdraftTo: creditLimit,
	})
}

// GetCreditLimit returns the org's overdraft allowance, defaulting to zero.
func (s *LedgerService) GetCreditLimit(ctx context.Context, orgID string) (money.Amount, error) {
	var limit money.Amount
	err := s.client.Pool.QueryRow(ctx, `
		SELECT credit_limit FROM wallet_settings WHERE org_id = $1;
	`, orgID).Scan(&limit)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return money.Zero(""), nil
		}
		return money.Amount{}, fmt.Errorf("ledger: failed to read credit limit: %w", err)
	}
	return limit, nil
}

// HasSpendableBalance reports whether an org's balance plus credit limit is
// strictly positive, i.e. whether it may start another billable request.
func (s *LedgerService) HasSpendableBalance(ctx context.Context, orgID string) (bool, money.Amount, error) {
	balance, err := s.GetBalance(ctx, orgID)
	if err != nil {
		return false, money.Amount{}, err
	}
	creditLimit, err := s.GetCreditLimit(ctx, orgID)
	if err != nil {
		return false, balance, err
	}
	available, err := balance.Add(money.FromMicros(creditLimit.Micros(), balance.Currency()))
	if err != nil {
		return false, balance, err
	}
	return available.IsPositive(), balance, nil
}
