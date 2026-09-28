// Package testutil provides helpers shared by integration tests.
package testutil

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ayeus/ayeusann/internal/db"
)

// DefaultTestDatabaseURL points at a database separate from the development
// one: integration tests insert users, orgs and hosts, and used to leave
// hundreds of rows in the dev database.
const DefaultTestDatabaseURL = "postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann_test?sslmode=disable"

// DB connects to the migrated test database. When it is unreachable the test
// is skipped, unless SN_REQUIRE_DB is set (as in CI), where it fails instead so
// a broken database can never turn a red build green.
func DB(t *testing.T) *db.Client {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = DefaultTestDatabaseURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := db.NewClient(ctx, db.Config{URL: url, MaxConns: 10, MinConns: 1})
	if err != nil {
		if os.Getenv("SN_REQUIRE_DB") != "" {
			t.Fatalf("test database unavailable: %v", err)
		}
		t.Skipf("skipping: test database unavailable (%v); run `make test-db` to create it", err)
	}
	t.Cleanup(client.Close)
	return client
}
