package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shambu2k/maintainer-bot/internal/intent"
)

// testNow matches the fixed instant used by the intent tests.
var testNow = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

func testGrant(overrides ...func(*intent.Grant)) intent.Grant {
	g := intent.Grant{
		RunID:        "run-1",
		Repo:         intent.Repo{Owner: "shambu2k", Name: "repo", AccountID: "acct-1"},
		Scope:        intent.Scope{Kind: intent.ScopeIssue, Number: 5, BaseRef: "main", HeadSHA: "abc123"},
		AllowedKinds: []intent.Kind{intent.KindOpenPR},
		TokenScope:   intent.TokenContentsWrite,
		Limits:       intent.DefaultLimits(),
		IssuedAt:     testNow.Add(-time.Minute),
		ExpiresAt:    testNow.Add(time.Hour),
	}
	for _, f := range overrides {
		f(&g)
	}
	return g
}

// ---- fakes ----

type fakeStore struct {
	resolve func(ctx context.Context, accountID string, scope intent.TokenScope) (string, error)
	calls   []struct {
		accountID string
		scope     intent.TokenScope
	}
}

func (f *fakeStore) Resolve(ctx context.Context, accountID string, scope intent.TokenScope) (string, error) {
	f.calls = append(f.calls, struct {
		accountID string
		scope     intent.TokenScope
	}{accountID, scope})
	if f.resolve == nil {
		return "pat-<redacted>", nil
	}
	return f.resolve(ctx, accountID, scope)
}

type fakeLedger struct {
	lookup func(ctx context.Context, key string) (string, bool, error)
	record func(ctx context.Context, key, runID, ref string) error
	gotKey string
	gotRef string
}

func (f *fakeLedger) Lookup(ctx context.Context, key string) (string, bool, error) {
	if f.lookup == nil {
		return "", false, nil
	}
	return f.lookup(ctx, key)
}

func (f *fakeLedger) Record(ctx context.Context, key, runID, ref string) error {
	f.gotKey, f.gotRef = key, ref
	if f.record == nil {
		return nil
	}
	return f.record(ctx, key, runID, ref)
}

type fakeWriter struct {
	openPR    func(ctx context.Context, cred Credential, spec OpenPRWrite) (string, error)
	updatePR  func(ctx context.Context, cred Credential, spec UpdatePRWrite) (string, error)
	postRev   func(ctx context.Context, cred Credential, spec PostReviewWrite) (string, error)
	postCmnt  func(ctx context.Context, cred Credential, spec PostCommentWrite) (string, error)
	setLabels func(ctx context.Context, cred Credential, spec SetLabelsWrite) (string, error)

	lastOpenPR *OpenPRWrite
	callCount   int
}

func (f *fakeWriter) OpenPR(ctx context.Context, c Credential, s OpenPRWrite) (string, error) {
	f.callCount++
	f.lastOpenPR = &s
	if f.openPR == nil {
		return "shambu2k/repo#123", nil
	}
	return f.openPR(ctx, c, s)
}
func (f *fakeWriter) UpdatePR(ctx context.Context, c Credential, s UpdatePRWrite) (string, error) {
	f.callCount++
	if f.updatePR == nil {
		return "shambu2k/repo#9", nil
	}
	return f.updatePR(ctx, c, s)
}
func (f *fakeWriter) PostReview(ctx context.Context, c Credential, s PostReviewWrite) (string, error) {
	f.callCount++
	if f.postRev == nil {
		return "shambu2k/repo#9", nil
	}
	return f.postRev(ctx, c, s)
}
func (f *fakeWriter) PostComment(ctx context.Context, c Credential, s PostCommentWrite) (string, error) {
	f.callCount++
	if f.postCmnt == nil {
		return "shambu2k/repo#5", nil
	}
	return f.postCmnt(ctx, c, s)
}
func (f *fakeWriter) SetLabels(ctx context.Context, c Credential, s SetLabelsWrite) (string, error) {
	f.callCount++
	if f.setLabels == nil {
		return "shambu2k/repo#5", nil
	}
	return f.setLabels(ctx, c, s)
}

type fakeDiff struct {
	diff func(ctx context.Context, runID, worktree string) (intent.Diff, error)
}

func (f *fakeDiff) FromWorktree(ctx context.Context, runID, worktree string) (intent.Diff, error) {
	if f.diff == nil {
		return intent.Diff{Files: []string{"go.mod"}, AddedLines: 3}, nil
	}
	return f.diff(ctx, runID, worktree)
}

func newExecutor(store *fakeStore, ledger *fakeLedger, writer *fakeWriter, diffs *fakeDiff, now time.Time) *Executor {
	return NewExecutor(store, ledger, writer, diffs, func() time.Time { return now })
}

func mustEnv(t *testing.T, g intent.Grant, kind intent.Kind, payload any) intent.Envelope {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return intent.Envelope{SchemaVersion: int(intent.SchemaVersion), RunID: g.RunID, Kind: kind, Payload: b}
}

func assertErr(t *testing.T, err error, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// ---------- Executor is the enforcement point ----------

func TestExecuteRevalidatesEnvelope(t *testing.T) {
	// Even if the worker "pre-checked" it, the executor re-validates before any
	// token resolution or write. An open_pr under a pull_request scope must not
	// slip through.
	g := testGrant(func(g *intent.Grant) {
		g.Scope = intent.Scope{Kind: intent.ScopePullRequest, Number: 9, BaseRef: "main", HeadSHA: "x"}
	})
	env := mustEnv(t, g, intent.KindOpenPR, map[string]any{"title": "x", "worktree": "cmd"})
	w := &fakeWriter{}
	ex := newExecutor(&fakeStore{}, &fakeLedger{}, w, &fakeDiff{}, testNow)
	_, err := ex.Execute(context.Background(), env, g)
	assertErr(t, err, intent.ErrScopeMismatch)
	if w.callCount != 0 {
		t.Fatalf("writer called %d times on rejected intent", w.callCount)
	}
}

func TestExecuteRejectsUngrantedKind(t *testing.T) {
	g := testGrant(func(g *intent.Grant) { g.AllowedKinds = []intent.Kind{intent.KindPostComment} })
	env := mustEnv(t, g, intent.KindOpenPR, map[string]any{"title": "x", "worktree": "cmd"})
	w := &fakeWriter{}
	ex := newExecutor(&fakeStore{}, &fakeLedger{}, w, &fakeDiff{}, testNow)
	_, err := ex.Execute(context.Background(), env, g)
	assertErr(t, err, intent.ErrCapabilityMissing)
	if w.callCount != 0 {
		t.Fatalf("writer called on ungranted intent")
	}
}

// ---------- OpenPR flow ----------

func TestExecuteOpenPRFlow(t *testing.T) {
	g := testGrant()
	env := mustEnv(t, g, intent.KindOpenPR, map[string]any{
		"title": "Bump acme to v1.1", "body": "thanks @bob", "worktree": "cmd/bump", "topic": "bump-dep",
	})
	store := &fakeStore{}
	led := &fakeLedger{}
	w := &fakeWriter{}
	ex := newExecutor(store, led, w, &fakeDiff{}, testNow)

	res, err := ex.Execute(context.Background(), env, g)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Deduped {
		t.Fatal("should not be deduped on first run")
	}
	if res.GitHubRef != "shambu2k/repo#123" {
		t.Fatalf("ref = %q", res.GitHubRef)
	}
	// writer got the grant-derived branch and base, never agent-chosen
	if w.lastOpenPR == nil {
		t.Fatal("OpenPR not called")
	}
	if w.lastOpenPR.Branch != "bot/run-1-bump-dep" {
		t.Errorf("branch = %q, want bot/run-1-bump-dep", w.lastOpenPR.Branch)
	}
	if w.lastOpenPR.Base != "main" {
		t.Errorf("base = %q, want main", w.lastOpenPR.Base)
	}
	// the mention survived sanitisation as a backticked (non-notifying) form
	if !strings.Contains(w.lastOpenPR.Body, "`@bob`") {
		t.Errorf("body not sanitised: %q", w.lastOpenPR.Body)
	}
	// store resolved the correct tier; ledger recorded the ref
	if len(store.calls) != 1 || store.calls[0].scope != intent.TokenContentsWrite {
		t.Fatalf("store calls = %+v, want one contents_write", store.calls)
	}
	if led.gotKey == "" {
		t.Fatal("ledger.Record not called")
	}
	if led.gotRef != "shambu2k/repo#123" {
		t.Fatalf("ledger ref = %q", led.gotRef)
	}
}

func TestExecuteBranchDefaultsWhenNoTopic(t *testing.T) {
	g := testGrant()
	env := mustEnv(t, g, intent.KindOpenPR, map[string]any{
		"title": "x", "worktree": "cmd",
	})
	w := &fakeWriter{}
	ex := newExecutor(&fakeStore{}, &fakeLedger{}, w, &fakeDiff{}, testNow)
	if _, err := ex.Execute(context.Background(), env, g); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if w.lastOpenPR.Branch != "bot/run-1-change" {
		t.Errorf("branch = %q, want bot/run-1-change", w.lastOpenPR.Branch)
	}
}

func TestExecuteOpenPRDeniedDiff(t *testing.T) {
	g := testGrant()
	env := mustEnv(t, g, intent.KindOpenPR, map[string]any{"title": "x", "worktree": "cmd"})
	diffs := &fakeDiff{diff: func(ctx context.Context, runID, wt string) (intent.Diff, error) {
		return intent.Diff{Files: []string{".github/workflows/deploy.yml"}}, nil
	}}
	w := &fakeWriter{}
	ex := newExecutor(&fakeStore{}, &fakeLedger{}, w, diffs, testNow)
	_, err := ex.Execute(context.Background(), env, g)
	assertErr(t, err, intent.ErrDeniedPath)
	if w.callCount != 0 {
		t.Fatal("writer called despite denied diff")
	}
}

// ---------- Idempotency ----------

func TestExecuteDedupesOnLedgerHit(t *testing.T) {
	g := testGrant()
	env := mustEnv(t, g, intent.KindOpenPR, map[string]any{"title": "x", "worktree": "cmd"})
	led := &fakeLedger{lookup: func(ctx context.Context, key string) (string, bool, error) {
		return "shambu2k/repo#123", true, nil
	}}
	w := &fakeWriter{}
	ex := newExecutor(&fakeStore{}, led, w, &fakeDiff{}, testNow)
	res, err := ex.Execute(context.Background(), env, g)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !res.Deduped {
		t.Fatal("expected dedup")
	}
	if res.GitHubRef != "shambu2k/repo#123" {
		t.Fatalf("ref = %q", res.GitHubRef)
	}
	if w.callCount != 0 {
		t.Fatal("writer called on dedup")
	}
	if led.gotRef != "" {
		t.Fatal("ledger.Record should not run on dedup")
	}
}

// ---------- Token resolution ----------

func TestExecuteTokenResolutionFailureStopsWrite(t *testing.T) {
	g := testGrant()
	env := mustEnv(t, g, intent.KindOpenPR, map[string]any{"title": "x", "worktree": "cmd"})
	store := &fakeStore{resolve: func(ctx context.Context, a string, s intent.TokenScope) (string, error) {
		return "", errors.New("keyring unreachable")
	}}
	w := &fakeWriter{}
	ex := newExecutor(store, &fakeLedger{}, w, &fakeDiff{}, testNow)
	_, err := ex.Execute(context.Background(), env, g)
	if err == nil || !strings.Contains(err.Error(), "resolve token") {
		t.Fatalf("err = %v, want wrapped resolve token error", err)
	}
	if w.callCount != 0 {
		t.Fatal("writer called despite token resolution failure")
	}
}

func TestExecuteTokenScopeDrivesResolveCall(t *testing.T) {
	g := testGrant(func(g *intent.Grant) {
		g.AllowedKinds = []intent.Kind{intent.KindSetLabels}
		g.TokenScope = intent.TokenIssuesWrite
		g.Scope = intent.Scope{Kind: intent.ScopeIssue, Number: 5}
	})
	env := mustEnv(t, g, intent.KindSetLabels, map[string]any{"add": []string{"kind/upgrade"}})
	store := &fakeStore{}
	ex := newExecutor(store, &fakeLedger{}, &fakeWriter{}, &fakeDiff{}, testNow)
	if _, err := ex.Execute(context.Background(), env, g); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(store.calls) != 1 || store.calls[0].scope != intent.TokenIssuesWrite {
		t.Fatalf("store calls = %+v, want issues_write", store.calls)
	}
}

// ---------- Other kinds ----------

func TestExecutePostReviewUsesScopeNumber(t *testing.T) {
	g := testGrant(func(g *intent.Grant) {
		g.AllowedKinds = []intent.Kind{intent.KindPostReview}
		g.TokenScope = intent.TokenIssuesWrite
		g.Scope = intent.Scope{Kind: intent.ScopePullRequest, Number: 42, BaseRef: "main", HeadSHA: "x"}
	})
	env := mustEnv(t, g, intent.KindPostReview, map[string]any{
		"verdict": "comment", "summary": "looks fine", "comments": []any{},
	})
	w := &fakeWriter{}
	ex := newExecutor(&fakeStore{}, &fakeLedger{}, w, &fakeDiff{}, testNow)
	if _, err := ex.Execute(context.Background(), env, g); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if w.callCount != 1 {
		t.Fatalf("writer call count = %d, want 1", w.callCount)
	}
}

func TestExecutePostCommentEmptyBodyStopsBeforeWrite(t *testing.T) {
	g := testGrant(func(g *intent.Grant) {
		g.AllowedKinds = []intent.Kind{intent.KindPostComment}
		g.TokenScope = intent.TokenIssuesWrite
	})
	env := intent.Envelope{SchemaVersion: int(intent.SchemaVersion), RunID: g.RunID, Kind: intent.KindPostComment,
		Payload: json.RawMessage(`{"body":"   "}`)}
	w := &fakeWriter{}
	ex := newExecutor(&fakeStore{}, &fakeLedger{}, w, &fakeDiff{}, testNow)
	_, err := ex.Execute(context.Background(), env, g)
	assertErr(t, err, intent.ErrMalformed)
	if w.callCount != 0 {
		t.Fatal("writer called for empty comment")
	}
}

// ---------- Credential redaction ----------

func TestCredentialStringRedactsToken(t *testing.T) {
	c := Credential{AccountID: "acct-1", Scope: intent.TokenContentsWrite, Token: "ghp_supersecretpatvalue", Repo: intent.Repo{Owner: "o", Name: "r"}}
	s := c.String()
	if strings.Contains(s, "ghp_supersecretpatvalue") {
		t.Fatalf("String() leaked the PAT: %q", s)
	}
	if !strings.Contains(s, "<redacted>") {
		t.Fatalf("String() should mark the token redacted: %q", s)
	}
}
