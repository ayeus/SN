package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/ayeus/ayeusann/internal/domain"
	"github.com/ayeus/ayeusann/internal/money"
	"github.com/jackc/pgx/v5"
)

var (
	ErrInsufficientBalance = errors.New("ledger: insufficient balance for debit")
	ErrInvalidDelta        = errors.New("ledger: delta cannot be zero")
	ErrOrgNotFound         = errors.New("ledger: organization not found")
	ErrDuplicateRef        = errors.New("ledger: a transaction with this reference already exists")
)

// LedgerService manages wallet transactions and ledger entries.
//
// Invariants:
//   - The ledger is append-only. Corrections are compensating entries.
//   - Every entry records the running balance after it is applied.
//   - Amounts are exact decimals, never floats.
//   - Concurrent transactions for one org are serialized by an advisory lock on
//     the org, so two simultaneous debits cannot both read the same prior balance.
type LedgerService struct {
	client *Client
}

// NewLedgerService creates a new LedgerService.
func NewLedgerService(client *Client) *LedgerService {
	return &LedgerService{client: client}
}

// GetBalance returns the current wallet balance for an organization.
// Ordering is by the monotonic seq column: the previous implementation ordered by
// (created_at DESC, entry_id DESC) where entry_id is a random UUID, so entries
// sharing a timestamp had no deterministic "latest".
func (s *LedgerService) GetBalance(ctx context.Context, orgID string) (money.Amount, error) {
	var balance money.Amount
	var currency string

	query := `
		SELECT balance_after, currency
		FROM wallet_ledger
		WHERE org_id = $1
		ORDER BY seq DESC
		LIMIT 1;
	`
	err := s.client.Pool.QueryRow(ctx, query, orgID).Scan(&balance, &currency)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return money.Zero("USD"), nil
		}
		return money.Amount{}, fmt.Errorf("ledger: failed to get balance: %w", err)
	}

	return money.FromMicros(balance.Micros(), currency), nil
}

// TransactionRequest describes a wallet movement.
type TransactionRequest struct {
	OrgID string
	// Delta is positive for credits and negative for debits.
	Delta money.Amount
	Kind  string
	// RefID links the entry to its source (payment intent, usage request).
	// When set, it is enforced unique per kind so retries cannot double-apply.
	RefID       *string
	Description *string
	// AllowOverdraftTo permits the balance to fall to this limit (a non-negative
	// magnitude). Zero means no overdraft. This replaces the old boolean
	// allowNegativeBalance flag, which let balances fall without bound.
	AllowOverdraftTo money.Amount
}

// RecordTransaction atomically records a credit or debit.
//
// The org's ledger is serialized with pg_advisory_xact_lock, taken before the
// balance is read. The prior implementation used SELECT ... FOR UPDATE on the
// newest row, which locks nothing when an org has no entries yet, so concurrent
// first debits both saw a zero balance and both succeeded.
func (s *LedgerService) RecordTransaction(ctx context.Context, req TransactionRequest) (*domain.WalletLedger, error) {
	if req.Delta.IsZero() {
		return nil, ErrInvalidDelta
	}
	if req.OrgID == "" {
		return nil, ErrOrgNotFound
	}
	if req.AllowOverdraftTo.IsNegative() {
		return nil, errors.New("ledger: overdraft limit must be expressed as a non-negative magnitude")
	}

	var entry domain.WalletLedger

	err := s.client.ExecTx(ctx, func(tx pgx.Tx) error {
		// Serialize all wallet movements for this org for the duration of the
		// transaction. hashtextextended gives a stable 64-bit key from the UUID.
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1, 0));`, req.OrgID,
		); err != nil {
			return fmt.Errorf("ledger: failed to acquire org lock: %w", err)
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
				return fmt.Errorf("ledger: failed to check for duplicate reference: %w", err)
			}
			if exists {
				return ErrDuplicateRef
			}
		}

		currentBalance := money.Zero("USD")
		var currency string
		err := tx.QueryRow(ctx, `
			SELECT balance_after, currency
			FROM wallet_ledger
			WHERE org_id = $1
			ORDER BY seq DESC
			LIMIT 1;
		`, req.OrgID).Scan(&currentBalance, &currency)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("ledger: failed to read current balance: %w", err)
			}
			currency = "USD"
			if err := tx.QueryRow(ctx,
				`SELECT currency FROM organizations WHERE id = $1;`, req.OrgID,
			).Scan(&currency); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrOrgNotFound
				}
				return fmt.Errorf("ledger: failed to read org currency: %w", err)
			}
		}

		current := money.FromMicros(currentBalance.Micros(), currency)
		delta := money.FromMicros(req.Delta.Micros(), currency)

		newBalance, err := current.Add(delta)
		if err != nil {
			return fmt.Errorf("ledger: balance arithmetic failed: %w", err)
		}

		// Enforce the overdraft floor.
		floor := money.FromMicros(-req.AllowOverdraftTo.Micros(), currency)
		if newBalance.LessThan(floor) {
			return fmt.Errorf("%w: balance %s plus delta %s would breach the %s floor",
				ErrInsufficientBalance, current, delta, floor)
		}

		insertQuery := `
			INSERT INTO wallet_ledger (org_id, delta, balance_after, kind, ref_id, description, currency)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING entry_id, org_id, delta, balance_after, kind, ref_id, description, currency, created_at;
		`
		return tx.QueryRow(ctx, insertQuery,
			req.OrgID, delta, newBalance, req.Kind, req.RefID, req.Description, currency,
		).Scan(
			&entry.EntryID,
			&entry.OrgID,
			&entry.Delta,
			&entry.BalanceAfter,
			&entry.Kind,
			&entry.RefID,
			&entry.Description,
			&entry.Currency,
			&entry.CreatedAt,
		)
	})

	if err != nil {
		return nil, err
	}

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
			return money.Zero("USD"), nil
		}
		return money.Amount{}, fmt.Errorf("ledger: failed to read credit limit: %w", err)
	}
	return limit, nil
}

// HasSpendableBalance reports whether an org can pay for at least a minimum
// charge, taking its credit limit into account.
func (s *LedgerService) HasSpendableBalance(ctx context.Context, orgID string, minimum money.Amount) (bool, error) {
	balance, err := s.GetBalance(ctx, orgID)
	if err != nil {
		return false, err
	}
	creditLimit, err := s.GetCreditLimit(ctx, orgID)
	if err != nil {
		return false, err
	}

	available, err := balance.Add(money.FromMicros(creditLimit.Micros(), balance.Currency()))
	if err != nil {
		return false, err
	}

	cmp, err := available.Cmp(money.FromMicros(minimum.Micros(), balance.Currency()))
	if err != nil {
		return false, err
	}
	return cmp >= 0, nil
}
