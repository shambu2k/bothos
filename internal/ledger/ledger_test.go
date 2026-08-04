package ledger

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/shambu2k/maintainer-bot/internal/intent"
)

const testDB = "maintbot_test"

// ensureTestDB creates the test database (the maintbot role is cluster
// superuser in the official image) so tests never touch the dev DB.
func ensureTestDB(t *testing.T) string {
	t.Helper()
	adminDSN := os.Getenv("TEST_ADMIN_DATABASE_URL")
	if adminDSN == "" {
		adminDSN = "postgres://maintbot:maintbot-dev@localhost:5432/postgres"
	}
	conn, err := pgx.Connect(context.Background(), adminDSN)
	if err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	defer conn.Close(context.Background())
	var exists bool
	if err := conn.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, testDB).Scan(&exists); err != nil {
		t.Fatalf("check db: %v", err)
	}
	if !exists {
		if _, err := conn.Exec(context.Background(), "CREATE DATABASE "+testDB); err != nil {
			t.Fatalf("create db: %v", err)
		}
	}
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://maintbot:maintbot-dev@localhost:5432/" + testDB
	}
	return dsn
}

func newTestStore(t *testing.T) *Postgres {
	t.Helper()
	dsn := ensureTestDB(t)
	ctx := context.Background()
	st, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		t.Fatalf("migrate: %v", err)
	}
	// clean slate between tests
	for _, tbl := range []string{"intents", "runs", "findings", "capability_gaps"} {
		if _, err := st.pool.Exec(ctx, "TRUNCATE "+tbl+" CASCADE"); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}
	t.Cleanup(st.Close)
	return st
}

func TestMigrateIdempotent(t *testing.T) {
	st := newTestStore(t)
	// second migrate must be a no-op
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestIntentLookupMissThenRecordThenHit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	insertRun(t, st, "run-1")

	if _, ok, err := st.Lookup(ctx, "key-1"); err != nil || ok {
		t.Fatalf("miss: ok=%v err=%v", ok, err)
	}
	if err := st.Record(ctx, "key-1", "run-1", "shambu2k/repo#123"); err != nil {
		t.Fatalf("record: %v", err)
	}
	ref, ok, err := st.Lookup(ctx, "key-1")
	if err != nil || !ok {
		t.Fatalf("hit: ok=%v err=%v", ok, err)
	}
	if ref != "shambu2k/repo#123" {
		t.Fatalf("ref = %q", ref)
	}
}

func TestIntentRecordIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	insertRun(t, st, "run-1")
	if err := st.Record(ctx, "key-x", "run-1", "shambu2k/repo#1"); err != nil {
		t.Fatalf("first record: %v", err)
	}
	// second record for the same key does not duplicate and does not clobber
	if err := st.Record(ctx, "key-x", "run-2", "shambu2k/repo#2"); err != nil {
		t.Fatalf("second record: %v", err)
	}
	ref, ok, _ := st.Lookup(ctx, "key-x")
	if !ok || ref != "shambu2k/repo#1" {
		t.Fatalf("expected original ref preserved, got ok=%v ref=%q", ok, ref)
	}
}

func TestInsertRunAndStatusTransition(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	g, _ := json.Marshal(&intent.Grant{RunID: "run-1"})
	run := Run{
		ID: "run-1", RepoID: "r1", Trigger: "webhook_pull_request",
		ScopeKind: "pull_request", ScopeNumber: 9,
		Grant: g, Decision: "allow", Status: RunQueued,
	}
	if err := st.InsertRun(ctx, run); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	// executor dedup keys off intents.idempotency_key; runs are referenced by
	// intents.run_id via foreign key — recording an intent for the run must work.
	if err := st.Record(ctx, "k", "run-1", "shambu2k/repo#9"); err != nil {
		t.Fatalf("record intent under run: %v", err)
	}
	if err := st.SetRunStatus(ctx, "run-1", RunSucceeded); err != nil {
		t.Fatalf("set status: %v", err)
	}

	var status string
	var ended bool
	if err := st.pool.QueryRow(ctx,
		`SELECT status, ended_at IS NOT NULL FROM runs WHERE id=$1`, "run-1").Scan(&status, &ended); err != nil {
		t.Fatalf("read: %v", err)
	}
	if status != "succeeded" || !ended {
		t.Fatalf("status=%q ended=%v", status, ended)
	}
}

func TestRecordCapabilityGap(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	st.InsertRun(ctx, Run{ID: "run-1", RepoID: "r", Trigger: "scheduled", ScopeKind: "scheduled", Grant: []byte("{}"), Decision: "allow", Status: RunQueued})
	if err := st.RecordCapabilityGap(ctx, "run-1", "KindNuke", "agent tried a verb outside the set"); err != nil {
		t.Fatalf("record gap: %v", err)
	}
}

// insertRun seeds a parent runs row so intents (which FK to runs) can be
// recorded — the real dispatch flow always inserts the run first.
func insertRun(t *testing.T, st *Postgres, id string) {
	t.Helper()
	if err := st.InsertRun(context.Background(), Run{
		ID: id, RepoID: "r", Trigger: "scheduled",
		ScopeKind: "scheduled", Grant: []byte("{}"), Decision: "allow", Status: RunQueued,
	}); err != nil {
		t.Fatalf("seed run %s: %v", id, err)
	}
}
