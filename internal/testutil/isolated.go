package testutil

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ayeus/ayeusann/internal/db"
	"github.com/jackc/pgx/v5"
)

// NewIsolatedDB creates a private, migrated and seeded database for one test
// package and returns it with a function that drops it. The scheduler and the
// coordinator run loops over every row in a table; in the shared test database
// they would act on the fixtures of packages running in parallel.
//
// Call it from TestMain. It returns a nil client when Postgres is unreachable
// and SN_REQUIRE_DB is unset, so the package's tests can skip.
func NewIsolatedDB(name string) (*db.Client, func(), error) {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		base = DefaultTestDatabaseURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		if os.Getenv("SN_REQUIRE_DB") != "" {
			return nil, nil, fmt.Errorf("test database unavailable: %w", err)
		}
		return nil, func() {}, nil
	}
	defer admin.Close(ctx)

	dbName := fmt.Sprintf("ayeusann_test_%s_%d", name, os.Getpid())
	if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+dbName+` WITH (FORCE)`); err != nil {
		return nil, nil, err
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		return nil, nil, fmt.Errorf("create %s: %w", dbName, err)
	}
	drop := func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if conn, err := pgx.Connect(c, base); err == nil {
			_, _ = conn.Exec(c, `DROP DATABASE IF EXISTS `+dbName+` WITH (FORCE)`)
			_ = conn.Close(c)
		}
	}

	u, err := url.Parse(base)
	if err != nil {
		drop()
		return nil, nil, err
	}
	u.Path = "/" + dbName

	if err := applySchema(ctx, u.String()); err != nil {
		drop()
		return nil, nil, err
	}
	client, err := db.NewClient(ctx, db.Config{URL: u.String(), MaxConns: 10, MinConns: 1})
	if err != nil {
		drop()
		return nil, nil, err
	}
	return client, func() { client.Close(); drop() }, nil
}

// applySchema runs every up migration, then the seed files.
func applySchema(ctx context.Context, dbURL string) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	migrations, _ := filepath.Glob(filepath.Join(root, "schema", "migrations", "*.up.sql"))
	seeds, _ := filepath.Glob(filepath.Join(root, "schema", "seeds", "*.sql"))
	sort.Strings(migrations)
	sort.Strings(seeds)
	if len(migrations) == 0 {
		return fmt.Errorf("no migrations found under %s", root)
	}

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	for _, f := range append(migrations, seeds...) {
		sql, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		// The simple protocol accepts a file of several statements.
		if _, err := conn.Exec(ctx, string(sql), pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above the working directory")
		}
		dir = parent
	}
}
