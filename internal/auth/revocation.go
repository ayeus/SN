package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrTokenAlreadyUsed is returned when a single-use token is redeemed twice.
var ErrTokenAlreadyUsed = errors.New("auth: token has already been used")

// RevocationStore tracks revoked and consumed token IDs. Stateless JWTs cannot
// be invalidated on their own, so logout, credential rotation, and single-use
// host registration all depend on this.
type RevocationStore interface {
	// Revoke marks a token ID as invalid until its natural expiry.
	Revoke(ctx context.Context, jti string, expiresAt time.Time, reason string) error
	// IsRevoked reports whether a token ID has been revoked.
	IsRevoked(ctx context.Context, jti string) (bool, error)
	// Consume atomically marks a single-use token as spent. It returns
	// ErrTokenAlreadyUsed if the token was already redeemed.
	Consume(ctx context.Context, jti string, expiresAt time.Time) error
	// RevokeAllForUser invalidates every token issued to a user before now.
	RevokeAllForUser(ctx context.Context, userID string, reason string) error
	// IssuedBefore reports the cutoff after which a user's tokens are valid.
	// Tokens issued before this instant are rejected.
	IssuedBefore(ctx context.Context, userID string) (time.Time, error)
}

// ─── Postgres implementation ──────────────────────────────────

// PGRevocationStore persists revocations in Postgres so they survive restarts
// and are shared across service replicas.
type PGRevocationStore struct {
	pool *pgxpool.Pool
}

// NewPGRevocationStore creates a Postgres-backed revocation store.
func NewPGRevocationStore(pool *pgxpool.Pool) *PGRevocationStore {
	return &PGRevocationStore{pool: pool}
}

func (s *PGRevocationStore) Revoke(ctx context.Context, jti string, expiresAt time.Time, reason string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO revoked_tokens (jti, expires_at, reason)
		VALUES ($1, $2, $3)
		ON CONFLICT (jti) DO NOTHING;
	`, jti, expiresAt, reason)
	if err != nil {
		return fmt.Errorf("auth: failed to revoke token: %w", err)
	}
	return nil
}

func (s *PGRevocationStore) IsRevoked(ctx context.Context, jti string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM revoked_tokens WHERE jti = $1);
	`, jti).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("auth: failed to check revocation: %w", err)
	}
	return exists, nil
}

// Consume records a single-use token as spent. The unique constraint on jti is
// what makes this atomic under concurrency: two simultaneous redemptions of the
// same registration token produce one success and one ErrTokenAlreadyUsed.
func (s *PGRevocationStore) Consume(ctx context.Context, jti string, expiresAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO revoked_tokens (jti, expires_at, reason)
		VALUES ($1, $2, 'consumed')
		ON CONFLICT (jti) DO NOTHING;
	`, jti, expiresAt)
	if err != nil {
		return fmt.Errorf("auth: failed to consume token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTokenAlreadyUsed
	}
	return nil
}

func (s *PGRevocationStore) RevokeAllForUser(ctx context.Context, userID, reason string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO token_invalidations (user_id, invalidated_at, reason)
		VALUES ($1, NOW(), $2)
		ON CONFLICT (user_id) DO UPDATE SET
			invalidated_at = NOW(),
			reason = EXCLUDED.reason;
	`, userID, reason)
	if err != nil {
		return fmt.Errorf("auth: failed to revoke user tokens: %w", err)
	}
	return nil
}

func (s *PGRevocationStore) IssuedBefore(ctx context.Context, userID string) (time.Time, error) {
	var cutoff time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT invalidated_at FROM token_invalidations WHERE user_id = $1;
	`, userID).Scan(&cutoff)
	if err != nil {
		// No invalidation recorded: zero time means "nothing is cut off".
		return time.Time{}, nil //nolint:nilerr // absence is the common case
	}
	return cutoff, nil
}

// PurgeExpired deletes revocation records whose tokens have already expired.
// Once a token is past its exp it is rejected by signature validation anyway,
// so retaining the row serves no purpose. Run this periodically.
func (s *PGRevocationStore) PurgeExpired(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM revoked_tokens WHERE expires_at < NOW();`)
	if err != nil {
		return 0, fmt.Errorf("auth: failed to purge expired revocations: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ─── In-memory implementation (tests and single-node dev) ─────

// MemoryRevocationStore is an in-process RevocationStore for tests and local
// development. It is not shared across processes; production must use Postgres.
type MemoryRevocationStore struct {
	mu            sync.RWMutex
	revoked       map[string]time.Time
	invalidations map[string]time.Time
}

// NewMemoryRevocationStore creates an in-memory revocation store.
func NewMemoryRevocationStore() *MemoryRevocationStore {
	return &MemoryRevocationStore{
		revoked:       make(map[string]time.Time),
		invalidations: make(map[string]time.Time),
	}
}

func (s *MemoryRevocationStore) Revoke(_ context.Context, jti string, expiresAt time.Time, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked[jti] = expiresAt
	return nil
}

func (s *MemoryRevocationStore) IsRevoked(_ context.Context, jti string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.revoked[jti]
	return ok, nil
}

func (s *MemoryRevocationStore) Consume(_ context.Context, jti string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.revoked[jti]; ok {
		return ErrTokenAlreadyUsed
	}
	s.revoked[jti] = expiresAt
	return nil
}

func (s *MemoryRevocationStore) RevokeAllForUser(_ context.Context, userID, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidations[userID] = time.Now()
	return nil
}

func (s *MemoryRevocationStore) IssuedBefore(_ context.Context, userID string) (time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.invalidations[userID], nil
}
