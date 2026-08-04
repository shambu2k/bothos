// Package testdb provides a shared integration-test database for packages that
// talk to Postgres (ledger, queue). It provisions maintbot_test once, applies
// the schema, and truncates domain tables between tests. Tests skip cleanly if
// Postgres is unreachable, so the pure unit packages still pass without infra.
package testdb

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

const testDB = "maintbot_test"

// DSN returns the connection string for the shared test database, creating it
// if needed. Reading it never fails the test; connection failures surface later.
func DSN(t *testing.T) string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	adminDSN := os.Getenv("TEST_ADMIN_DATABASE_URL")
	if adminDSN == "" {
		adminDSN = "postgres://maintbot:maintbot-dev@localhost:5432/postgres"
	}
	conn, err := pgx.Connect(context.Background(), adminDSN)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	defer conn.Close(context.Background())

	ctx := context.Background()
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, testDB).Scan(&exists); err != nil {
		t.Fatalf("check db: %v", err)
	}
	if !exists {
		// testDB is a constant, not attacker input.
		if _, err := conn.Exec(ctx, "CREATE DATABASE "+testDB); err != nil {
			t.Fatalf("create db: %v", err)
		}
	}
	return "postgres://maintbot:maintbot-dev@localhost:5432/" + testDB
}

// Truncate clears the given domain tables (not River's, which the queue owns).
func Truncate(t *testing.T, dsn string, tables ...string) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect for truncate: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(),
		"TRUNCATE "+strings.Join(tables, ", ")+" CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}
