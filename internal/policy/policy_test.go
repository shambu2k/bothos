package policy

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shambu2k/bothos/internal/intent"
)

var now = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

func baseRules() Rules {
	return Rules{
		Enabled:        true,
		AllowedLabels:  []string{"kind/upgrade"},
		ActorAllowlist: []string{"shambu2k"},
		DeniedPaths:    []string{"secrets/**"},
	}
}

func scheduled(a ...func(*Trigger)) Trigger {
	t := Trigger{
		Kind:          TriggerScheduled,
		RepoID:        "repo-1",
		AccountID:     "acct-1",
		Owner:         "shambu2k",
		Name:          "repo",
		DefaultBranch: "main",
	}
	for _, f := range a {
		f(&t)
	}
	return t
}

func assertErr(t *testing.T, err error, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestScheduledYieldsOpenPRGrant(t *testing.T) {
	g, err := Decide(scheduled(), baseRules(), "run-1", now)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if g.Scope.Kind != intent.ScopeScheduled || g.Scope.BaseRef != "main" {
		t.Fatalf("scope = %+v", g.Scope)
	}
	if !contains(g.AllowedKinds, intent.KindOpenPR) {
		t.Fatalf("scheduled run should be able to open PRs, got %v", g.AllowedKinds)
	}
	if g.TokenScope != intent.TokenContentsWrite {
		t.Fatalf("token = %v, want contents_write", g.TokenScope)
	}
	if len(g.DeniedPaths) == 0 || !strings.Contains(strings.Join(g.DeniedPaths, ","), "secrets/**") {
		t.Fatalf("denied paths not carried through: %v", g.DeniedPaths)
	}
	if g.ExpiresAt.Before(now) {
		t.Fatalf("grant already expired at %v", g.ExpiresAt)
	}
}

func TestScheduledCannotReview(t *testing.T) {
	g, _ := Decide(scheduled(), baseRules(), "run-1", now)
	if contains(g.AllowedKinds, intent.KindPostReview) {
		t.Fatalf("scheduled run must not be able to review: %v", g.AllowedKinds)
	}
}

func TestPullRequestYieldReadOnlyReview(t *testing.T) {
	g, err := Decide(Trigger{
		Kind: TriggerPullRequest, RepoID: "r", Owner: "o", Name: "n",
		Number: 9, BaseRef: "main", HeadSHA: "abc",
	}, baseRules(), "run-2", now)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if g.Scope.Kind != intent.ScopePullRequest || g.Scope.Number != 9 {
		t.Fatalf("scope = %+v", g.Scope)
	}
	if g.TokenScope != intent.TokenReadOnly {
		t.Fatalf("review token = %v, want read_only", g.TokenScope)
	}
	// read-only: exactly one kind, the review
	if len(g.AllowedKinds) != 1 || g.AllowedKinds[0] != intent.KindPostReview {
		t.Fatalf("kinds = %v, want [post_review]", g.AllowedKinds)
	}
}

func TestIssueLabeledAllowsOpenPR(t *testing.T) {
	g, err := Decide(Trigger{
		Kind: TriggerIssueLabeled, Owner: "o", Name: "n", Number: 5,
		Actor: "shambu2k", LabelsApplied: []string{"kind/upgrade"},
	}, baseRules(), "run-3", now)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if g.Scope.Kind != intent.ScopeIssue || g.Scope.Number != 5 {
		t.Fatalf("scope = %+v", g.Scope)
	}
	if g.TokenScope != intent.TokenContentsWrite {
		t.Fatalf("token = %v, want contents_write", g.TokenScope)
	}
}

func TestIssueLabeledDeniedLabel(t *testing.T) {
	_, err := Decide(Trigger{
		Kind: TriggerIssueLabeled, Owner: "o", Name: "n", Number: 5,
		Actor: "shambu2k", LabelsApplied: []string{"kind/whatever"},
	}, baseRules(), "run-3", now)
	assertErr(t, err, ErrPolicyDenied)
}

func TestIssueLabeledUnauthorizedActor(t *testing.T) {
	_, err := Decide(Trigger{
		Kind: TriggerIssueLabeled, Owner: "o", Name: "n", Number: 5,
		Actor: "attacker", LabelsApplied: []string{"kind/upgrade"},
	}, baseRules(), "run-3", now)
	assertErr(t, err, ErrPolicyDenied)
}

func TestIssueLabeledActorWithWriteBypassesAllowlist(t *testing.T) {
	// "Actor ∈ actor_allowlist (or has write permission on the repo)"
	g, err := Decide(Trigger{
		Kind: TriggerIssueLabeled, Owner: "o", Name: "n", Number: 5,
		Actor: "collaborator", ActorHasWrite: true, LabelsApplied: []string{"kind/upgrade"},
	}, baseRules(), "run-3", now)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !contains(g.AllowedKinds, intent.KindOpenPR) {
		t.Fatalf("write-capable actor should be allowed: %v", g.AllowedKinds)
	}
}

func TestDisabledRepoDeniesAll(t *testing.T) {
	r := baseRules()
	r.Enabled = false
	_, err := Decide(scheduled(), r, "run-1", now)
	assertErr(t, err, ErrPolicyDenied)
}

func contains(k []intent.Kind, v intent.Kind) bool {
	for _, x := range k {
		if x == v {
			return true
		}
	}
	return false
}
