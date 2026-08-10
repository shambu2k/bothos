package upgrade

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/queue"
	"github.com/shambu2k/bothos/internal/testdb"
)

func TestGrantForUpgrade(t *testing.T) {
	g := GrantForUpgrade("r1", intent.Repo{Owner: "acme", Name: "repo", AccountID: "acme"}, "main")

	if g.Scope.Kind != intent.ScopeScheduled || g.Scope.BaseRef != "main" {
		t.Fatalf("scope: %+v", g.Scope)
	}
	if len(g.AllowedKinds) != 1 || g.AllowedKinds[0] != "open_pr" {
		t.Fatalf("allowed kinds: %v", g.AllowedKinds)
	}
	if g.TokenScope != intent.TokenContentsWrite {
		t.Fatalf("token scope: %q", g.TokenScope)
	}
	if g.Repo.AccountID != "acme" {
		t.Fatalf("account: %q", g.Repo.AccountID)
	}
	if !g.ExpiresAt.After(time.Now()) {
		t.Fatal("grant should be unexpired at issue")
	}
	if len(g.DeniedPaths) == 0 {
		t.Fatal("grant should deny secret paths by default")
	}
}

func newScheduler(t *testing.T) *Scheduler {
	t.Helper()
	dsn := testdb.DSN(t)
	ctx := context.Background()
	st, err := ledger.New(ctx, dsn)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		t.Fatalf("migrate: %v", err)
	}
	q, err := queue.Open(ctx, dsn, nil, nil)
	if err != nil {
		st.Close()
		t.Fatalf("queue: %v", err)
	}
	testdb.Truncate(t, dsn, "runs")
	t.Cleanup(func() { q.Close(); st.Close() })
	return &Scheduler{Ledger: st, Queue: q}
}

// TestScheduleOneRunPerRepo asserts the new trigger-not-mint contract: exactly
// one security run per repo, suppressed while one is outstanding, resumed after
// a terminal failure.
func TestScheduleOneRunPerRepo(t *testing.T) {
	s := newScheduler(t)
	ctx := context.Background()

	// Not enabled -> nothing, no error.
	n, err := s.Schedule(ctx, "acme", "repo", "main", false)
	if err != nil || n != 0 {
		t.Fatalf("disabled: n=%d err=%v", n, err)
	}

	// Enabled -> exactly one run, with scope=security + the recorded base.
	n, err = s.Schedule(ctx, "acme", "repo", "main", true)
	if err != nil || n != 1 {
		t.Fatalf("first schedule: n=%d err=%v (want 1)", n, err)
	}
	run, err := s.Ledger.RunByID(ctx, runIDFor(t, s))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var m UpgradeMeta
	if err := json.Unmarshal(run.Meta, &m); err != nil {
		t.Fatalf("meta: %v", err)
	}
	if m.Scope != "security" || m.BaseRef != "main" {
		t.Fatalf("meta = %+v, want scope=security base_ref=main", m)
	}

	// A second schedule is suppressed while the run is outstanding.
	n, err = s.Schedule(ctx, "acme", "repo", "main", true)
	if err != nil || n != 0 {
		t.Fatalf("second schedule: n=%d err=%v (want 0, suppressed)", n, err)
	}

	// After a terminal failure, a new run is allowed.
	if err := s.Ledger.SetRunStatus(ctx, run.ID, ledger.RunFailed); err != nil {
		t.Fatalf("fail run: %v", err)
	}
	n, err = s.Schedule(ctx, "acme", "repo", "main", true)
	if err != nil || n != 1 {
		t.Fatalf("resume after failure: n=%d err=%v (want 1)", n, err)
	}

	// A succeeded run is terminal and must NOT block the next schedule —
	// regression: 3 succeeded runs blocked re-scheduling forever.
	run2, err := s.Ledger.RunByID(ctx, runIDFor(t, s))
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	if err := s.Ledger.SetRunStatus(ctx, run2.ID, ledger.RunSucceeded); err != nil {
		t.Fatalf("succeed run: %v", err)
	}
	n, err = s.Schedule(ctx, "acme", "repo", "main", true)
	if err != nil || n != 1 {
		t.Fatalf("resume after success: n=%d err=%v (want 1)", n, err)
	}
}

func runIDFor(t *testing.T, s *Scheduler) string {
	t.Helper()
	var id string
	if err := s.Queue.Pool().QueryRow(context.Background(),
		`SELECT id FROM runs WHERE trigger='upgrade' ORDER BY created_at DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("query run id: %v", err)
	}
	return id
}
