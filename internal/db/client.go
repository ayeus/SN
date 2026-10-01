// Package db provides database access, connection pooling, and transactional helpers.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config holds database connection parameters.
type Config struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnIdleTime time.Duration
	MaxConnLifetime time.Duration
}

// Client wraps pgxpool.Pool and provides database operations.
type Client struct {
	Pool *pgxpool.Pool
}

// NewClient initializes a pgxpool.Pool from the provided config and verifies connection.
func NewClient(ctx context.Context, cfg Config) (*Client, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("db: failed to parse connection string: %w", err)
	}

	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	} else {
		poolCfg.MaxConns = 25
	}

	if cfg.MinConns > 0 {
		poolCfg.MinConns = cfg.MinConns
	} else {
		poolCfg.MinConns = 5
	}

	if cfg.MaxConnIdleTime > 0 {
		poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	} else {
		poolCfg.MaxConnIdleTime = 15 * time.Minute
	}

	if cfg.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	} else {
		poolCfg.MaxConnLifetime = 1 * time.Hour
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("db: failed to create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: failed to ping database: %w", err)
	}

	return &Client{Pool: pool}, nil
}

// Close closes the database connection pool.
func (c *Client) Close() {
	if c.Pool != nil {
		c.Pool.Close()
	}
}

// Ping checks if the database is reachable.
func (c *Client) Ping(ctx context.Context) error {
	return c.Pool.Ping(ctx)
}

// ExecTx executes fn within a database transaction, rolling back if fn returns
// an error.
//
// A transaction that Postgres aborts as a deadlock victim (40P01) or for a
// serialization failure (40001) did nothing wrong and is safe to run again, so
// it is retried a few times. Without this a lost race silently dropped the
// update: a replica's stage event vanished and the replica looked stuck.
// fn may therefore run more than once and must not have side effects outside
// the transaction.
func (c *Client) ExecTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if err = c.execTxOnce(ctx, fn); err == nil || !retryable(err) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(attempt+1) * 15 * time.Millisecond):
		}
	}
	return err
}

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001")
}

func (c *Client) execTxOnce(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: failed to begin transaction: %w", err)
	}

	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: failed to commit transaction: %w", err)
	}

	return nil
}
