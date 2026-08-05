package ledger

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/scan"
	"github.com/shambu2k/bothos/internal/testdb"
)

func newTestStore(t *testing.T) *Postgres {
	t.Helper()
	dsn := testdb.DSN(t)
	ctx := context.Background()
	st, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		t.Fatalf("migrate: %v", err)
	}
	testdb.Truncate(t, dsn, "intents", "runs", "findings", "capability_gaps")
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

func TestUpsertFindingsIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	insertRun(t, st, "run-1")
	insertRun(t, st, "run-2")

	f1 := scan.Finding{Scanner: scan.ScannerOSV, Ecosystem: "npm", Package: "leftpad",
		CurrentVersion: "0.0.1", TargetVersion: "0.0.2", Severity: "HIGH", AdvisoryID: "GHSA-abc", RepoID: "shambu2k/repo"}
	f2 := scan.Finding{Scanner: scan.ScannerOSV, Ecosystem: "Go", Package: "golang.org/x/net",
		CurrentVersion: "v0.20.0", TargetVersion: "v0.32.0", AdvisoryID: "GO-2024-1", RepoID: "shambu2k/repo"}

	if err := st.UpsertFindings(ctx, "run-1", []scan.Finding{f1, f2}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := st.Findings(ctx, "shambu2k/repo")
	if err != nil || len(got) != 2 {
		t.Fatalf("after first upsert: len=%d err=%v", len(got), err)
	}

	// same finding, refreshed version -> same row updated, not duplicated
	f1.TargetVersion = "0.0.3"
	if err := st.UpsertFindings(ctx, "run-2", []scan.Finding{f1}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, err = st.Findings(ctx, "shambu2k/repo")
	if err != nil || len(got) != 2 {
		t.Fatalf("after re-upsert: len=%d err=%v (must not duplicate)", len(got), err)
	}
	for _, g := range got {
		if g.AdvisoryID == "GHSA-abc" && g.TargetVersion != "0.0.3" {
			t.Fatalf("existing row not updated in place: %+v", g)
		}
	}
}

func TestUpsertUpdatesIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	insertRun(t, st, "run-1")

	u1 := scan.Update{RepoID: "shambu2k/repo", Ecosystem: "npm", Package: "express",
		CurrentVersion: "4.17.0", TargetVersion: "4.19.0", UpdateType: "minor"}
	u2 := scan.Update{RepoID: "shambu2k/repo", Ecosystem: "npm", Package: "tar",
		CurrentVersion: "7.5.16", TargetVersion: "7.5.19", UpdateType: "patch"}

	if err := st.UpsertUpdates(ctx, "run-1", []scan.Update{u1, u2}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got := countUpdates(t, st); got != 2 {
		t.Fatalf("after first upsert: want 2, got %d", got)
	}

	// same package, refreshed target -> updated in place, not duplicated
	u1.TargetVersion = "4.20.0"
	if err := st.UpsertUpdates(ctx, "run-1", []scan.Update{u1}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if got := countUpdates(t, st); got != 2 {
		t.Fatalf("re-upsert must not duplicate: want 2, got %d", got)
	}
	var tv string
	if err := st.pool.QueryRow(ctx,
		`SELECT target_version FROM updates WHERE repo_id='shambu2k/repo' AND package='express'`).Scan(&tv); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if tv != "4.20.0" {
		t.Fatalf("existing row not refreshed: got %q", tv)
	}
}

func countUpdates(t *testing.T, st *Postgres) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(context.Background(), `SELECT count(*) FROM updates`).Scan(&n); err != nil {
		t.Fatalf("count updates: %v", err)
	}
	return n
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
