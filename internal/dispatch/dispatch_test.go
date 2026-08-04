package dispatch

import (
	"context"
	"testing"

	"github.com/google/go-github/v69/github"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/policy"
	"github.com/shambu2k/bothos/internal/queue"
	"github.com/shambu2k/bothos/internal/testdb"
)

func baseRules(ctx context.Context, owner, name string) (policy.Rules, error) {
	return policy.Rules{
		Enabled:        true,
		AllowedLabels:  []string{"kind/upgrade"},
		ActorAllowlist: []string{"shambu2k"},
	}, nil
}

func newTestEnv(t *testing.T) (*Dispatcher, *ledger.Postgres, *queue.Queue) {
	t.Helper()
	ctx := context.Background()
	dsn := testdb.DSN(t)

	l, err := ledger.New(ctx, dsn)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	t.Cleanup(l.Close)
	if err := l.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	q, err := queue.Open(ctx, dsn, nil, nil)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	t.Cleanup(q.Close)

	testdb.Truncate(t, dsn, "runs", "intents", "river_job")

	d := New(l, q, baseRules)
	d.newRun = func() string { return "run-fixed" }
	return d, l, q
}

func labeledIssue(actor, label string, number int) *github.IssuesEvent {
	return &github.IssuesEvent{
		Action: github.String("labeled"),
		Issue:  &github.Issue{Number: github.Int(number)},
		Label:  &github.Label{Name: github.String(label)},
		Sender: &github.User{Login: github.String(actor)},
		Repo: &github.Repository{
			Owner: &github.User{Login: github.String("shambu2k")},
			Name:  github.String("repo"),
		},
	}
}

func TestAllowLabeledIssueRecordsRunAndEnqueues(t *testing.T) {
	ctx := context.Background()
	d, _, q := newTestEnv(t)

	if err := d.HandleEvent(ctx, labeledIssue("shambu2k", "kind/upgrade", 5)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	var decision, status string
	var scopeKind string
	var scopeNum int
	if err := q.Pool().QueryRow(ctx,
		`SELECT decision, status, scope_kind, scope_number FROM runs WHERE id='run-fixed'`).
		Scan(&decision, &status, &scopeKind, &scopeNum); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if decision != "allow" || status != string(ledger.RunQueued) {
		t.Fatalf("decision=%q status=%q, want allow/queued", decision, status)
	}
	if scopeKind != "issue" || scopeNum != 5 {
		t.Fatalf("scope=%s/%d, want issue/5", scopeKind, scopeNum)
	}

	var jobs int
	if err := q.Pool().QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE args->>'run_id' = 'run-fixed'`).Scan(&jobs); err != nil {
		t.Fatalf("read jobs: %v", err)
	}
	if jobs != 1 {
		t.Fatalf("enqueued jobs = %d, want 1", jobs)
	}
}

func TestDeniedLabeledIssueRecordsRunButNoJob(t *testing.T) {
	ctx := context.Background()
	d, _, q := newTestEnv(t)

	// unauthorized actor applying the trigger label
	if err := d.HandleEvent(ctx, labeledIssue("attacker", "kind/upgrade", 5)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	var decision, status, deny string
	if err := q.Pool().QueryRow(ctx,
		`SELECT decision, status, deny_reason FROM runs WHERE id='run-fixed'`).
		Scan(&decision, &status, &deny); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if decision != "deny" || status != string(ledger.RunDenied) {
		t.Fatalf("decision=%q status=%q, want deny/denied", decision, status)
	}
	if deny == "" {
		t.Fatal("expected a deny reason")
	}

	var jobs int
	if err := q.Pool().QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE args->>'run_id' = 'run-fixed'`).Scan(&jobs); err != nil {
		t.Fatalf("read jobs: %v", err)
	}
	if jobs != 0 {
		t.Fatalf("denied run must not enqueue, got %d jobs", jobs)
	}
}

func TestDisallowedLabelDenied(t *testing.T) {
	ctx := context.Background()
	d, _, q := newTestEnv(t)
	if err := d.HandleEvent(ctx, labeledIssue("shambu2k", "kind/other", 5)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	var decision string
	if err := q.Pool().QueryRow(ctx,
		`SELECT decision FROM runs WHERE id='run-fixed'`).Scan(&decision); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if decision != "deny" {
		t.Fatalf("decision=%q, want deny", decision)
	}
}

func TestPullRequestEventDispatched(t *testing.T) {
	ctx := context.Background()
	d, _, q := newTestEnv(t)

	ev := &github.PullRequestEvent{
		Action: github.String("opened"),
		Number: github.Int(9),
		Repo: &github.Repository{
			Owner: &github.User{Login: github.String("shambu2k")},
			Name:  github.String("repo"),
		},
		PullRequest: &github.PullRequest{
			Base: &github.PullRequestBranch{Ref: github.String("main")},
			Head: &github.PullRequestBranch{Ref: github.String("feat"), SHA: github.String("abc123")},
		},
	}
	if err := d.HandleEvent(ctx, ev); err != nil {
		t.Fatalf("handle: %v", err)
	}

	var decision, scopeKind string
	if err := q.Pool().QueryRow(ctx,
		`SELECT decision, scope_kind FROM runs WHERE id='run-fixed'`).Scan(&decision, &scopeKind); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if decision != "allow" || scopeKind != "pull_request" {
		t.Fatalf("decision=%q scope=%q", decision, scopeKind)
	}
}

func TestUnhandledEventIsNoOp(t *testing.T) {
	ctx := context.Background()
	d, _, q := newTestEnv(t)
	if err := d.HandleEvent(ctx, &github.PingEvent{}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	var n int
	if err := q.Pool().QueryRow(ctx, `SELECT count(*) FROM runs`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("ping should not record a run, got %d", n)
	}
}
