package dispatch

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/go-github/v69/github"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/policy"
	"github.com/shambu2k/bothos/internal/queue"
	"github.com/shambu2k/bothos/internal/runtime"
	"github.com/shambu2k/bothos/internal/testdb"
)

func baseRules(ctx context.Context, owner, name string) (policy.Rules, error) {
	return policy.Rules{
		Enabled:        true,
		AllowedLabels:  []string{"kind/upgrade"},
		ActorAllowlist: []string{"shambu2k"},
		AutoReview:     true,
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

	d := New(l, q, baseRules,
		func(context.Context, string, string, string) (bool, error) { return true, nil },
		func(context.Context, string, string, int) (string, string, error) {
			return "base-loaded", "head-loaded", nil
		},
	)
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

func TestLabeledIssueChecksActorWritePermission(t *testing.T) {
	authorizeCalls := 0
	d := &Dispatcher{
		authorize: func(_ context.Context, owner, name, actor string) (bool, error) {
			authorizeCalls++
			if owner != "shambu2k" || name != "repo" || actor != "maintainer" {
				t.Fatalf("authorization args = %s/%s %s", owner, name, actor)
			}
			return true, nil
		},
	}

	trigger, handled, err := d.triggerFromEvent(context.Background(), labeledIssue("maintainer", "kind/upgrade", 5))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if authorizeCalls != 1 || !trigger.ActorHasWrite {
		t.Fatalf("trigger=%+v authorization calls=%d", trigger, authorizeCalls)
	}
}

func TestAllowLabeledIssueRecordsIssueTaskMetadata(t *testing.T) {
	ctx := context.Background()
	d, _, q := newTestEnv(t)
	event := labeledIssue("shambu2k", "kind/upgrade", 5)
	event.Issue.Title = github.String("Repair login validation")
	event.Issue.Body = github.String("The login form incorrectly accepts empty passwords.")

	if err := d.HandleEvent(ctx, event); err != nil {
		t.Fatal(err)
	}

	var raw []byte
	if err := q.Pool().QueryRow(ctx, `SELECT meta FROM runs WHERE id='run-fixed'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var task runtime.IssueTask
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatal(err)
	}
	if task.IssueNumber != 5 || task.Title != event.Issue.GetTitle() || task.Body != event.Issue.GetBody() || task.BaseRef != "" {
		t.Fatalf("issue task = %+v", task)
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

func TestLabeledIssueAuthorizationFailureRecordsDenyWithoutJob(t *testing.T) {
	ctx := context.Background()
	d, _, q := newTestEnv(t)
	d.authorize = func(context.Context, string, string, string) (bool, error) {
		return false, context.DeadlineExceeded
	}

	if err := d.HandleEvent(ctx, labeledIssue("maintainer", "kind/upgrade", 5)); err != nil {
		t.Fatal(err)
	}

	var decision string
	if err := q.Pool().QueryRow(ctx, `SELECT decision FROM runs WHERE id='run-fixed'`).Scan(&decision); err != nil {
		t.Fatal(err)
	}
	var jobs int
	if err := q.Pool().QueryRow(ctx, `SELECT count(*) FROM river_job WHERE args->>'run_id'='run-fixed'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if decision != "deny" || jobs != 0 {
		t.Fatalf("decision=%q jobs=%d", decision, jobs)
	}
}

func TestDeniedLabeledIssueRecordsRunButNoJob(t *testing.T) {
	ctx := context.Background()
	d, _, q := newTestEnv(t)
	d.authorize = func(context.Context, string, string, string) (bool, error) { return false, nil }

	// A non-allowlisted actor without repository write permission applies the trigger label.
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
			Base: &github.PullRequestBranch{Ref: github.String("main"), SHA: github.String("base123")},
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

func TestManualReviewLabelAuthorizesAndCapturesSHAs(t *testing.T) {
	authCalls := 0
	d := &Dispatcher{
		authorize: func(_ context.Context, owner, name, actor string) (bool, error) {
			authCalls++
			if owner != "shambu2k" || name != "repo" || actor != "maintainer" {
				t.Fatalf("authorization args = %s/%s %s", owner, name, actor)
			}
			return true, nil
		},
	}
	event := pullRequestLabelEvent("maintainer", "bothos/review")
	trigger, handled, err := d.triggerFromEvent(context.Background(), event)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if authCalls != 1 || !trigger.Manual || !trigger.ActorHasWrite ||
		trigger.BaseSHA != "base-sha" || trigger.HeadSHA != "head-sha" {
		t.Fatalf("trigger = %+v authCalls=%d", trigger, authCalls)
	}
}

func TestManualReviewMentionLoadsImmutableSHAs(t *testing.T) {
	loadCalls := 0
	d := &Dispatcher{
		authorize: func(context.Context, string, string, string) (bool, error) { return true, nil },
		loadPR: func(_ context.Context, owner, name string, number int) (string, string, error) {
			loadCalls++
			if owner != "shambu2k" || name != "repo" || number != 12 {
				t.Fatalf("loader args = %s/%s#%d", owner, name, number)
			}
			return "loaded-base", "loaded-head", nil
		},
	}
	trigger, handled, err := d.triggerFromEvent(context.Background(), issueCommentEvent("maintainer", "hello\n  @BoThOs   review  \nthanks", true))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if loadCalls != 1 || !trigger.Manual || trigger.BaseSHA != "loaded-base" || trigger.HeadSHA != "loaded-head" {
		t.Fatalf("trigger = %+v loadCalls=%d", trigger, loadCalls)
	}
}

func TestManualReviewMentionIgnoresNonCommands(t *testing.T) {
	d := &Dispatcher{
		authorize: func(context.Context, string, string, string) (bool, error) {
			t.Fatal("authorization called for ignored event")
			return false, nil
		},
	}
	tests := []struct {
		name  string
		event *github.IssueCommentEvent
	}{
		{name: "fix", event: issueCommentEvent("maintainer", "@bothos fix", true)},
		{name: "substring", event: issueCommentEvent("maintainer", "please run @bothos review now", true)},
		{name: "ordinary issue", event: issueCommentEvent("maintainer", "@bothos review", false)},
		{name: "bot", event: issueCommentEvent("dependabot[bot]", "@bothos review", true)},
	}
	tests[3].event.Sender.Type = github.String("Bot")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, handled, err := d.triggerFromEvent(context.Background(), tt.event); err != nil || handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
		})
	}
}

func TestManualReviewAuthorizationFailureRecordsDenyWithoutJob(t *testing.T) {
	ctx := context.Background()
	d, _, q := newTestEnv(t)
	d.authorize = func(context.Context, string, string, string) (bool, error) {
		return false, context.DeadlineExceeded
	}
	if err := d.HandleEvent(ctx, pullRequestLabelEvent("maintainer", "bothos/review")); err != nil {
		t.Fatal(err)
	}
	var decision string
	if err := q.Pool().QueryRow(ctx, `SELECT decision FROM runs WHERE id='run-fixed'`).Scan(&decision); err != nil {
		t.Fatal(err)
	}
	var jobs int
	if err := q.Pool().QueryRow(ctx, `SELECT count(*) FROM river_job WHERE args->>'run_id'='run-fixed'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if decision != "deny" || jobs != 0 {
		t.Fatalf("decision=%q jobs=%d", decision, jobs)
	}
}

func TestAutomaticPullRequestDoesNotAuthorizeActor(t *testing.T) {
	d := &Dispatcher{authorize: func(context.Context, string, string, string) (bool, error) {
		t.Fatal("automatic event performed actor lookup")
		return false, nil
	}}
	for _, action := range []string{"opened", "reopened", "synchronize"} {
		event := pullRequestLabelEvent("anyone", "")
		event.Action = github.String(action)
		trigger, handled, err := d.triggerFromEvent(context.Background(), event)
		if err != nil || !handled || trigger.Manual {
			t.Fatalf("%s trigger=%+v handled=%v err=%v", action, trigger, handled, err)
		}
	}
}

func pullRequestLabelEvent(actor, label string) *github.PullRequestEvent {
	return &github.PullRequestEvent{
		Action: github.String("labeled"),
		Number: github.Int(12),
		Label:  &github.Label{Name: github.String(label)},
		Sender: &github.User{Login: github.String(actor)},
		Repo: &github.Repository{
			Owner: &github.User{Login: github.String("shambu2k")},
			Name:  github.String("repo"),
		},
		PullRequest: &github.PullRequest{
			Base: &github.PullRequestBranch{Ref: github.String("main"), SHA: github.String("base-sha")},
			Head: &github.PullRequestBranch{Ref: github.String("feature"), SHA: github.String("head-sha")},
		},
	}
}

func issueCommentEvent(actor, body string, pullRequest bool) *github.IssueCommentEvent {
	issue := &github.Issue{Number: github.Int(12)}
	if pullRequest {
		issue.PullRequestLinks = &github.PullRequestLinks{}
	}
	return &github.IssueCommentEvent{
		Action:  github.String("created"),
		Issue:   issue,
		Comment: &github.IssueComment{Body: github.String(body)},
		Sender:  &github.User{Login: github.String(actor), Type: github.String("User")},
		Repo: &github.Repository{
			Owner: &github.User{Login: github.String("shambu2k")},
			Name:  github.String("repo"),
		},
	}
}

func TestAutomaticReviewGateDoesNotTreatLingeringLabelAsManual(t *testing.T) {
	ctx := context.Background()
	d, _, q := newTestEnv(t)
	d.rules = func(context.Context, string, string) (policy.Rules, error) {
		return policy.Rules{Enabled: true, AutoReview: false}, nil
	}
	event := pullRequestLabelEvent("maintainer", "")
	event.Action = github.String("synchronize")
	event.PullRequest.Labels = []*github.Label{{Name: github.String("bothos/review")}}
	if err := d.HandleEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	var decision string
	if err := q.Pool().QueryRow(ctx, `SELECT decision FROM runs WHERE id='run-fixed'`).Scan(&decision); err != nil {
		t.Fatal(err)
	}
	if decision != "deny" {
		t.Fatalf("synchronize decision = %q, want deny", decision)
	}
}

func TestManualReviewOverridesAutomaticGate(t *testing.T) {
	ctx := context.Background()
	d, _, q := newTestEnv(t)
	d.rules = func(context.Context, string, string) (policy.Rules, error) {
		return policy.Rules{Enabled: true, AutoReview: false}, nil
	}
	if err := d.HandleEvent(ctx, pullRequestLabelEvent("maintainer", "bothos/review")); err != nil {
		t.Fatal(err)
	}
	var decision string
	if err := q.Pool().QueryRow(ctx, `SELECT decision FROM runs WHERE id='run-fixed'`).Scan(&decision); err != nil {
		t.Fatal(err)
	}
	if decision != "allow" {
		t.Fatalf("manual decision = %q, want allow", decision)
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
