// Package intent defines the write-side contract between an untrusted agent
// runtime and the trusted GitHub executor.
//
// Design rule: an intent carries CONTENT, never TARGETING. The agent cannot
// name a repository, an installation, a base branch, a PR number, or an issue
// number. All of that is supplied by the executor from the Grant issued at
// dispatch time, before the agent ran. This is the single property that makes
// prompt injection from issue bodies and changelogs survivable: the worst a
// hijacked agent can do is write the wrong words in the right place.
package intent

import (
	"encoding/json"
	"time"
)

// SchemaVersion is bumped on any breaking change to payload shapes.
// The executor rejects envelopes it does not recognise rather than
// best-effort parsing them.
const SchemaVersion = 2

type Kind string

const (
	KindOpenPR      Kind = "open_pr"
	KindUpdatePR    Kind = "update_pr"
	KindPostReview  Kind = "post_review"
	KindPostComment Kind = "post_comment"
	KindSetLabels   Kind = "set_labels"
)

// AllKinds is the closed set. Adding a verb is a deliberate act: it requires a
// code change here, a validation rule in validate.go, and a policy update.
// A run that wants something outside this set fails with ErrCapabilityMissing
// and is recorded for human review.
var AllKinds = []Kind{
	KindOpenPR, KindUpdatePR, KindPostReview, KindPostComment, KindSetLabels,
}

// Envelope is what the agent runtime emits. Note the absence of any repo,
// owner, number, or ref field — that is not an oversight.
type Envelope struct {
	SchemaVersion int             `json:"schema_version"`
	RunID         string          `json:"run_id"`
	Kind          Kind            `json:"kind"`
	Payload       json.RawMessage `json:"payload"`

	// Sources lists the untrusted inputs that were in the agent's context when
	// it produced this intent. The agent self-reports these; they are not
	// trusted for enforcement, only recorded so that after a bad PR you can ask
	// "what was it reading?" without replaying the whole run.
	Sources []Source `json:"sources,omitempty"`
}

type Source struct {
	Type string `json:"type"` // issue_body | pr_body | review_comment | changelog | advisory
	Ref  string `json:"ref"`  // "#412", "https://…/CHANGELOG.md", "GHSA-xxxx"
}

// ---------- payloads ----------

// OpenPR does not carry a diff. The executor reads the sandbox worktree and
// computes the diff itself, so the agent cannot hand over a patch that differs
// from what it actually built and tested. It carries content only — branch,
// base, and repo are all resolved by the executor from git state and the
// grant, never from the agent.
type OpenPR struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Draft bool   `json:"draft"`
}

// UpdatePR targets the PR named in the Grant's Scope, not one of the agent's
// choosing.
type UpdatePR struct {
	Body  *string `json:"body,omitempty"`
	Draft *bool   `json:"draft,omitempty"`
}

type Verdict string

const (
	VerdictComment        Verdict = "comment"
	VerdictRequestChanges Verdict = "request_changes"
)

// There is no VerdictApprove, deliberately. If branch protection counts bot
// approvals toward required reviews, an approve capability silently becomes a
// merge capability — and the thing being reviewed is attacker-authored code.
// Approval is not representable in this type, so it cannot be reached by a
// policy misconfiguration either.

type PostReview struct {
	Verdict  Verdict         `json:"verdict"`
	Summary  string          `json:"summary"`
	Comments []ReviewComment `json:"comments"`
}

type ReviewComment struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Side     string `json:"side"` // LEFT | RIGHT
	Body     string `json:"body"`
	Verified bool   `json:"verified"`
	Evidence string `json:"evidence,omitempty"`
}

type PostComment struct {
	Body string `json:"body"`
}

type SetLabels struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

// ---------- grant ----------

// Grant is issued by the policy layer at dispatch, before the agent starts. It
// is immutable for the life of the run and is never visible to the agent.
type Grant struct {
	RunID string `json:"run_id"`

	Repo  Repo  `json:"repo"`
	Scope Scope `json:"scope"`

	AllowedKinds []Kind     `json:"allowed_kinds"`
	TokenScope   TokenScope `json:"token_scope"`
	Limits       Limits     `json:"limits"`

	// DeniedPaths are gitignore-style globs the resulting diff may not touch.
	// Deny wins over any allow. See defaultDeniedPaths in validate.go.
	DeniedPaths []string `json:"denied_paths"`

	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Repo struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`

	// AccountID is the resource owner (org or user) whose credential set covers
	// this repo. The executor resolves (AccountID, TokenScope) to a fine-grained
	// PAT from its keyring. The agent never sees any of this.
	AccountID string `json:"account_id"`
}

type ScopeKind string

const (
	ScopeIssue       ScopeKind = "issue"
	ScopePullRequest ScopeKind = "pull_request"
	ScopeScheduled   ScopeKind = "scheduled"
)

// Scope is what the run is *about*. Every intent is resolved against it.
type Scope struct {
	Kind    ScopeKind `json:"kind"`
	Number  int       `json:"number,omitempty"` // issue or PR number
	BaseRef string    `json:"base_ref"`
	BaseSHA string    `json:"base_sha"`
	HeadSHA string    `json:"head_sha"`
}

// TokenScope selects which credential the executor uses. Under machine-account
// auth each tier is a separate fine-grained PAT scoped to the same repo set with
// escalating permissions — not a token minted per run. The validation rules do
// not care which it is; only the executor's keyring does.
type TokenScope string

const (
	TokenReadOnly      TokenScope = "read_only"      // contents:read, pulls:read
	TokenIssuesWrite   TokenScope = "issues_write"   // + issues:write, pulls:write
	TokenContentsWrite TokenScope = "contents_write" // + contents:write
)

type Limits struct {
	MaxIntents     int `json:"max_intents"`
	MaxFiles       int `json:"max_files"`
	MaxDiffLines   int `json:"max_diff_lines"`
	MaxBodyBytes   int `json:"max_body_bytes"`
	MaxComments    int `json:"max_comments"`
	MaxLabelsAdded int `json:"max_labels_added"`
}

// DefaultLimits are deliberately tight. A dependency bump that wants to touch
// 200 files is not a dependency bump.
func DefaultLimits() Limits {
	return Limits{
		MaxIntents:     8,
		MaxFiles:       40,
		MaxDiffLines:   2000,
		MaxBodyBytes:   16 << 10,
		MaxComments:    25,
		MaxLabelsAdded: 5,
	}
}
