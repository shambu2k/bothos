// Package policy computes the immutable Grant for a run at dispatch time,
// before the agent runs. The grant is the complete capability surface for the
// run: nothing the agent reads afterwards can widen it. The plan calls this
// (with the intent schema and executor) "the product"; OPA/Cedar is a possible
// executor for these rules, but the decision logic lives here as data-driven
// Go so it is testable and audit-proof without extra machinery.
package policy

import (
	"errors"
	"fmt"
	"time"

	"github.com/shambu2k/bothos/internal/intent"
)

// ErrPolicyDenied is returned when a trigger is denied at dispatch (disabled
// repo, disallowed label, unauthorized actor). A denied run is still recorded
// in the ledger — the plan's Phase 4 exit is exactly this: a denied decision
// in the ledger and nothing else.
var ErrPolicyDenied = errors.New("trigger denied by policy")

type TriggerKind int

const (
	TriggerScheduled TriggerKind = iota
	TriggerPullRequest
	TriggerIssueLabeled
)

// String returns the ledger's trigger label (the runs.trigger column).
func (t TriggerKind) String() string {
	switch t {
	case TriggerPullRequest:
		return "webhook_pull_request"
	case TriggerIssueLabeled:
		return "webhook_issue_labeled"
	default:
		return "scheduled"
	}
}

// Trigger is the raw event the policy evaluates. It is the only unaudited
// input; everything else in the grant is fixed by these rules.
type Trigger struct {
	Kind          TriggerKind
	RepoID        string
	AccountID     string
	Owner         string
	Name          string
	Number        int    // PR or issue number, when applicable
	BaseRef       string // base ref for a PR
	BaseSHA       string
	HeadSHA       string
	DefaultBranch string
	Actor         string
	ActorHasWrite bool
	Manual        bool
	// ReviewLabel is true only when a pull-request review was triggered by the
	// bothos/review label rather than an exact review comment.
	ReviewLabel   bool
	LabelsApplied []string

	// IssueTitle and IssueBody are the webhook snapshot supplied to an issue
	// task as untrusted context. They carry no authority or targeting.
	IssueTitle string
	IssueBody  string
}

// Rules is the per-repo configuration (repo_config in the data model). The
// deny list and actor/label allowlists are data, evaluated here.
type Rules struct {
	Enabled        bool
	AllowedLabels  []string
	ActorAllowlist []string
	AutoReview     bool
	DeniedPaths    []string
}

// Decide computes the Grant for a trigger. An error (always wrapping
// ErrPolicyDenied) means the run must be denied and recorded, not retried or
// widened.
func Decide(t Trigger, r Rules, runID string, now time.Time) (intent.Grant, error) {
	if !r.Enabled {
		return intent.Grant{}, fmt.Errorf("%w: repo disabled", ErrPolicyDenied)
	}

	base := intent.Grant{
		RunID:       runID,
		Repo:        intent.Repo{Owner: t.Owner, Name: t.Name, AccountID: t.AccountID},
		DeniedPaths: r.DeniedPaths,
		Limits:      intent.DefaultLimits(),
		IssuedAt:    now,
		Manual:      t.Manual,
		ExpiresAt:   now.Add(time.Hour),
	}

	switch t.Kind {
	case TriggerScheduled:
		base.Scope = intent.Scope{Kind: intent.ScopeScheduled, BaseRef: t.DefaultBranch}
		base.AllowedKinds = []intent.Kind{intent.KindOpenPR}
		base.TokenScope = intent.TokenContentsWrite

	case TriggerPullRequest:
		if t.ReviewLabel && !t.Manual {
			return intent.Grant{}, fmt.Errorf("%w: review-label trigger without manual flag", ErrPolicyDenied)
		}
		if t.Manual && !t.ActorHasWrite {
			return intent.Grant{}, fmt.Errorf("%w: manual review actor %q lacks write permission", ErrPolicyDenied, t.Actor)
		}
		if t.ReviewLabel && !containsStr(r.ActorAllowlist, t.Actor) {
			return intent.Grant{}, fmt.Errorf("%w: review-label actor %q is not configured", ErrPolicyDenied, t.Actor)
		}
		if !t.Manual && !r.AutoReview {
			return intent.Grant{}, fmt.Errorf("%w: automatic review disabled", ErrPolicyDenied)
		}
		// Review is a read-only workload. Fork PRs execute untrusted code in a
		// sandbox with no write token, so the grant itself never exceeds
		// read_only — enforced structurally here.
		base.Scope = intent.Scope{Kind: intent.ScopePullRequest, Number: t.Number, BaseRef: t.BaseRef, BaseSHA: t.BaseSHA, HeadSHA: t.HeadSHA}
		base.AllowedKinds = []intent.Kind{intent.KindPostReview}
		base.TokenScope = intent.TokenReadOnly

	case TriggerIssueLabeled:
		if !has(t.LabelsApplied, r.AllowedLabels) {
			return intent.Grant{}, fmt.Errorf("%w: label %v not allowed", ErrPolicyDenied, t.LabelsApplied)
		}
		if !containsStr(r.ActorAllowlist, t.Actor) {
			return intent.Grant{}, fmt.Errorf("%w: actor %q is not configured for labeled issues", ErrPolicyDenied, t.Actor)
		}
		if !t.ActorHasWrite {
			return intent.Grant{}, fmt.Errorf("%w: labeled-issue actor %q lacks write permission", ErrPolicyDenied, t.Actor)
		}
		base.Scope = intent.Scope{Kind: intent.ScopeIssue, Number: t.Number, BaseRef: t.DefaultBranch}
		base.AllowedKinds = []intent.Kind{intent.KindOpenPR, intent.KindPostComment}
		base.TokenScope = intent.TokenContentsWrite

	default:
		return intent.Grant{}, fmt.Errorf("%w: unknown trigger", ErrPolicyDenied)
	}

	return base, nil
}

// has reports whether any applied label is in the allowlist.
func has(applied, allowed []string) bool {
	for _, a := range applied {
		for _, l := range allowed {
			if a == l {
				return true
			}
		}
	}
	return false
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
