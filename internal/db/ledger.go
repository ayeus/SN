package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/spazor/spazenode/internal/domain"
)

var (
	ErrInsufficientBalance = errors.New("ledger: insufficient balance for debit")
	ErrInvalidDelta        = errors.New("ledger: delta cannot be zero")
	ErrOrgNotFound         = errors.New("ledger: organization not found")
)

// LedgerService manages wallet transactions and ledger entries.
type LedgerService struct {
	client *Client
}

// NewLedgerService creates a new LedgerService.
func NewLedgerService(client *Client) *LedgerService {
	return &LedgerService{client: client}
}

// GetBalance returns the current wallet balance for an organization.
func (s *LedgerService) GetBalance(ctx context.Context, orgID string) (float64, error) {
	var balance float64
	query := `
		SELECT COALESCE(balance_after, 0)
		FROM wallet_ledger
		WHERE org_id = $1
		ORDER BY created_at DESC, entry_id DESC
		LIMIT 1;
	`
	err := s.client.Pool.QueryRow(ctx, query, orgID).Scan(&balance)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("ledger: failed to get balance: %w", err)
	}
	return balance, nil
}

// RecordTransaction atomically records a credit or debit in the wallet_ledger.
// Enforces balance checks for debits and returns the new entry and balance.
func (s *LedgerService) RecordTransaction(
	ctx context.Context,
	orgID string,
	delta float64,
	kind string,
	refID *string,
	description *string,
	allowNegativeBalance bool,
) (*domain.WalletLedger, error) {
	if delta == 0 {
		return nil, ErrInvalidDelta
	}

	var entry domain.WalletLedger

	err := s.client.ExecTx(ctx, func(tx pgx.Tx) error {
		// Lock the latest ledger entry for this org to prevent race conditions
		var currentBalance float64
		lockQuery := `
			SELECT balance_after
			FROM wallet_ledger
			WHERE org_id = $1
			ORDER BY created_at DESC, entry_id DESC
			LIMIT 1
			FOR UPDATE;
		`
		err := tx.QueryRow(ctx, lockQuery, orgID).Scan(&currentBalance)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("ledger: failed to lock ledger row: %w", err)
		}

		newBalance := currentBalance + delta

		if !allowNegativeBalance && newBalance < 0 {
			return ErrInsufficientBalance
		}

		insertQuery := `
			INSERT INTO wallet_ledger (org_id, delta, balance_after, kind, ref_id, description)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING entry_id, org_id, delta, balance_after, kind, ref_id, description, created_at;
		`
		err = tx.QueryRow(ctx, insertQuery, orgID, delta, newBalance, kind, refID, description).Scan(
			&entry.EntryID,
			&entry.OrgID,
			&entry.Delta,
			&entry.BalanceAfter,
			&entry.Kind,
			&entry.RefID,
			&entry.Description,
			&entry.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("ledger: failed to insert ledger entry: %w", err)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return &entry, nil
}
