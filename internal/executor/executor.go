// Package executor is the sole holder of GitHub credentials and the only
// process that writes to GitHub. Workers never hold a token; they emit
// content-only intents, and this package resolves them against the grant
// issued at dispatch time.
//
// The executor is deliberately dumb about policy (the grant is computed before
// the agent ran and is immutable) and deliberately strict about validation:
// it re-runs the full validation pipeline before touching a credential, so a
// bypass anywhere upstream still dies here.
package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/upgrade"
)

// Credential is a resolved fine-grained PAT bound to one resource owner at one
// tier. It is never logged; String() redacts the token.
type Credential struct {
	AccountID string
	Scope     intent.TokenScope
	Token     string
	Repo      intent.Repo
}

func (c Credential) String() string {
	return fmt.Sprintf("Credential{account=%s scope=%s repo=%s/%s token=<redacted>}",
		c.AccountID, c.Scope, c.Repo.Owner, c.Repo.Name)
}

// Write specs carry only *resolved* placement — the executor has already mapped
// the sanitised intent onto the grant's scope. No spec below carries an
// agent-chosen repo, branch base, or number.

type OpenPRWrite struct {
	Branch string
	Base   string
	Title  string
	Body   string
	Draft  bool
}

type UpdatePRWrite struct {
	PRNumber int
	Body     *string
	Draft    *bool
}

type PostReviewWrite struct {
	PRNumber int
	Verdict  intent.Verdict
	Summary  string
	Comments []intent.ReviewComment
}

type PostCommentWrite struct {
	Number int
	Body   string
}

type SetLabelsWrite struct {
	Number int
	Add    []string
	Remove []string
}

// CredentialStore resolves (account, tier) to a PAT. Backed by a keyring or
// secrets manager in production; the PAT never transits a worker or sandbox.
type CredentialStore interface {
	Resolve(ctx context.Context, accountID string, scope intent.TokenScope) (pat string, err error)
}

// Ledger records successful executions keyed by the derived idempotency key so
// a River retry or GitHub redelivery cannot open a second PR.
type Ledger interface {
	Lookup(ctx context.Context, idemKey string) (githubRef string, ok bool, err error)
	Record(ctx context.Context, idemKey, runID, githubRef string) error
}

// DiffSource computes the diff the sandbox worktree holds against its base.
type DiffSource interface {
	FromWorktree(ctx context.Context, runID, worktree string) (intent.Diff, error)
}

// GitHubWriter is the thin network adapter over go-github. Every write goes
// through here, and nowhere else.
type GitHubWriter interface {
	OpenPR(ctx context.Context, cred Credential, spec OpenPRWrite) (ref string, err error)
	UpdatePR(ctx context.Context, cred Credential, spec UpdatePRWrite) (ref string, err error)
	PostReview(ctx context.Context, cred Credential, spec PostReviewWrite) (ref string, err error)
	AcknowledgeReview(ctx context.Context, cred Credential, prNumber int) (ref string, err error)
	PostComment(ctx context.Context, cred Credential, spec PostCommentWrite) (ref string, err error)
	SetLabels(ctx context.Context, cred Credential, spec SetLabelsWrite) (ref string, err error)
	// PushBranch pushes a locally-committed work branch (in worktree) to the
	// remote under the credential. Only the executor calls this — it is the
	// single writer.
	PushBranch(ctx context.Context, cred Credential, branch, worktree string) error
}

type Result struct {
	Kind      intent.Kind
	GitHubRef string
	Deduped   bool
}

type Executor struct {
	store  CredentialStore
	ledger Ledger
	gh     GitHubWriter
	diffs  DiffSource
	now    func() time.Time
}

func NewExecutor(store CredentialStore, ledger Ledger, gh GitHubWriter, diffs DiffSource, now func() time.Time) *Executor {
	return &Executor{store: store, ledger: ledger, gh: gh, diffs: diffs, now: now}
}

// Execute validates the envelope against the grant, dedupes by derived
// idempotency key, resolves the tier-appropriate credential, and performs
// exactly one write. worktree is the sandbox worktree, passed out-of-band:
// the branch and base it must push/open against are read from git state, never
// from the envelope.
func (e *Executor) Execute(ctx context.Context, env intent.Envelope, g intent.Grant, worktree string) (Result, error) {
	// Enforcement point: re-validate even though the worker validated at
	// dispatch. The payload returned is the sanitised one — that is what gets
	// written, never the raw envelope.
	p, err := intent.Validate(env, g, e.now())
	if err != nil {
		return Result{}, err
	}

	key := intent.IdempotencyKey(env, g)
	if ref, ok, err := e.ledger.Lookup(ctx, key); err != nil {
		return Result{}, fmt.Errorf("ledger lookup: %w", err)
	} else if ok {
		return Result{Kind: env.Kind, GitHubRef: ref, Deduped: true}, nil
	}

	tokenScope := g.TokenScope
	if env.Kind == intent.KindPostReview {
		tokenScope = intent.TokenIssuesWrite
	}

	pat, err := e.store.Resolve(ctx, g.Repo.AccountID, tokenScope)
	if err != nil {
		return Result{}, fmt.Errorf("resolve token: %w", err)
	}
	cred := Credential{AccountID: g.Repo.AccountID, Scope: tokenScope, Token: pat, Repo: g.Repo}

	var ref string
	switch v := p.(type) {
	case intent.OpenPR:
		if err := e.checkWorktreeDiff(ctx, g, worktree); err != nil {
			return Result{}, err
		}
		// The branch and base both come from git state — the agent created the
		// branch, the clone's origin/HEAD names the base. Never transported.
		branch, err := upgrade.CurrentBranch(ctx, worktree)
		if err != nil {
			return Result{}, fmt.Errorf("current branch: %w", err)
		}
		if !strings.HasPrefix(branch, "bot/"+g.RunID+"-") {
			return Result{}, fmt.Errorf("agent branch %q must be bot/<runID>-*", branch)
		}
		base, err := upgrade.BaseBranch(ctx, worktree)
		if err != nil {
			return Result{}, fmt.Errorf("base branch: %w", err)
		}
		if err := e.gh.PushBranch(ctx, cred, branch, worktree); err != nil {
			return Result{}, fmt.Errorf("push branch: %w", err)
		}
		ref, err = e.gh.OpenPR(ctx, cred, OpenPRWrite{
			Branch: branch,
			Base:   base,
			Title:  v.Title,
			Body:   v.Body,
			Draft:  v.Draft,
		})

	case intent.UpdatePR:
		if worktree != "" {
			if err := e.checkWorktreeDiff(ctx, g, worktree); err != nil {
				return Result{}, err
			}
		}
		ref, err = e.gh.UpdatePR(ctx, cred, UpdatePRWrite{
			PRNumber: g.Scope.Number,
			Body:     v.Body,
			Draft:    v.Draft,
		})

	case intent.PostReview:
		ref, err = e.gh.PostReview(ctx, cred, PostReviewWrite{
			PRNumber: g.Scope.Number,
			Verdict:  v.Verdict,
			Summary:  v.Summary,
			Comments: v.Comments,
		})

	case intent.PostComment:
		ref, err = e.gh.PostComment(ctx, cred, PostCommentWrite{
			Number: g.Scope.Number,
			Body:   v.Body,
		})

	case intent.SetLabels:
		ref, err = e.gh.SetLabels(ctx, cred, SetLabelsWrite{
			Number: g.Scope.Number,
			Add:    v.Add,
			Remove: v.Remove,
		})

	default:
		return Result{}, intent.ErrUnknownKind
	}
	if err != nil {
		return Result{}, err
	}

	if err := e.ledger.Record(ctx, key, g.RunID, ref); err != nil {
		return Result{}, fmt.Errorf("ledger record: %w", err)
	}
	return Result{Kind: env.Kind, GitHubRef: ref}, nil
}

// AcknowledgeReview posts the trusted manual-review placeholder with the
// executor-only comment credential. The writer recovers an existing owned
// marker, making retries idempotent without exposing a write token upstream.
func (e *Executor) AcknowledgeReview(ctx context.Context, grant intent.Grant) error {
	if grant.Scope.Kind != intent.ScopePullRequest || grant.Scope.Number == 0 {
		return fmt.Errorf("acknowledge review requires pull-request scope")
	}
	pat, err := e.store.Resolve(ctx, grant.Repo.AccountID, intent.TokenIssuesWrite)
	if err != nil {
		return fmt.Errorf("resolve token: %w", err)
	}
	credential := Credential{
		AccountID: grant.Repo.AccountID,
		Scope:     intent.TokenIssuesWrite,
		Token:     pat,
		Repo:      grant.Repo,
	}
	if _, err := e.gh.AcknowledgeReview(ctx, credential, grant.Scope.Number); err != nil {
		return fmt.Errorf("acknowledge review: %w", err)
	}
	return nil
}

func (e *Executor) checkWorktreeDiff(ctx context.Context, g intent.Grant, worktree string) error {
	diff, err := e.diffs.FromWorktree(ctx, g.RunID, worktree)
	if err != nil {
		return fmt.Errorf("diff: %w", err)
	}
	if err := intent.ValidateDiff(diff, g); err != nil {
		return err
	}
	return nil
}
