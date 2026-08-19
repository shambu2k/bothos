package runpipe

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/review"
	"github.com/shambu2k/bothos/internal/runtime"
)

// ReviewSandboxer creates a tokenless detached worktree for one immutable PR head.
type ReviewSandboxer func(ctx context.Context, repo string, prNumber int, baseSHA, headSHA string) (runtime.Sandbox, error)

// ReviewPipeline orchestrates one deterministic, read-only pull-request review.
type ReviewPipeline struct {
	Store       Store
	Agent       runtime.AgentRuntime
	Acknowledge func(context.Context, intent.Grant) error
	Exec        Executor
	Sandbox     ReviewSandboxer
	Checks      func(context.Context, string) ([]review.Finding, error)
	// Now returns the current time (nil clock defaults to the wall clock;
	// tests inject it for deterministic grant-expiry and validate calls).
	Now func() time.Time
	// Log, when non-nil, receives structured run logs (each with a
	// run_id attribute); nil falls back to slog.Default().
	Log *slog.Logger
}

// now is the injected-clock seam used for the grant-expiry gate and the
// review-validate timestamp.
func (p *ReviewPipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// runLog returns a logger carrying runID as a structured run_id field,
// falling back to slog.Default() when no logger was injected.
func (p *ReviewPipeline) runLog(runID string) *slog.Logger {
	if p.Log != nil {
		return p.Log.With("run_id", runID)
	}
	return slog.Default().With("run_id", runID)
}

var classificationToken = regexp.MustCompile(`(?i)\[(verified|opinion)\]`)

// Run executes a review and returns the aggregate GitHub comment reference.
func (p *ReviewPipeline) Run(ctx context.Context, runID string) (string, error) {
	run, err := p.Store.RunByID(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("load run: %w", err)
	}
	var grant intent.Grant
	if err := json.Unmarshal(run.Grant, &grant); err != nil {
		return "", fmt.Errorf("grant: %w", err)
	}
	if err := p.Store.SetRunStatus(ctx, runID, ledger.RunRunning); err != nil {
		return "", err
	}
	fail := func(cause error) (string, error) {
		_ = p.Store.SetRunStatus(ctx, runID, ledger.RunFailed)
		_ = p.Store.SetRunFailure(ctx, runID, cause.Error())
		return "", cause
	}

	if grant.Scope.Kind != intent.ScopePullRequest || grant.Scope.Number == 0 ||
		strings.TrimSpace(grant.Scope.BaseSHA) == "" || strings.TrimSpace(grant.Scope.HeadSHA) == "" {
		return fail(fmt.Errorf("review grant requires pull-request number and immutable base/head SHAs"))
	}
	if p.now().After(grant.ExpiresAt) {
		return fail(fmt.Errorf("grant expired at %s (dispatched %s)", grant.ExpiresAt, grant.IssuedAt))
	}
	if grant.Manual && p.Acknowledge != nil {
		ackCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := p.Acknowledge(ackCtx, grant)
		cancel()
		if err != nil {
			p.runLog(runID).Warn("review acknowledgement failed", "err", err)
		}
	}

	sandbox, err := p.Sandbox(ctx, grant.Repo.Owner+"/"+grant.Repo.Name, grant.Scope.Number, grant.Scope.BaseSHA, grant.Scope.HeadSHA)
	if err != nil {
		return fail(fmt.Errorf("sandbox: %w", err))
	}
	checks := p.Checks
	if checks == nil {
		checks = review.All
	}
	findings, err := checks(ctx, sandbox.Worktree())
	if err != nil {
		return fail(fmt.Errorf("review checks: %w", err))
	}

	result, err := p.Agent.Run(ctx, runtime.RunInput{
		RunID: runID,
		Task: runtime.ReviewTask{
			PRNumber: grant.Scope.Number,
			BaseSHA:  grant.Scope.BaseSHA,
			HeadSHA:  grant.Scope.HeadSHA,
			RepoURL:  "https://github.com/" + grant.Repo.Owner + "/" + grant.Repo.Name + ".git",
		},
		Sandbox: sandbox,
		Limits:  runtime.Limits{MaxSeconds: time.Duration(runtime.AgentWallSeconds) * time.Second},
	})
	if err != nil {
		return fail(fmt.Errorf("agent: %w", err))
	}
	if err := verifyReviewWorktree(ctx, sandbox.Worktree(), grant.Scope.HeadSHA); err != nil {
		return fail(err)
	}
	if len(result.Intents) != 1 {
		return fail(fmt.Errorf("review agent produced %d intents, want exactly one post_review", len(result.Intents)))
	}
	for _, envelope := range result.Intents {
		if envelope.Kind != intent.KindPostReview {
			return fail(fmt.Errorf("review agent produced non-review intent %q", envelope.Kind))
		}
	}

	decoded, err := intent.Validate(result.Intents[0], grant, p.now())
	if err != nil {
		return fail(fmt.Errorf("review intent: %w", err))
	}
	model := decoded.(intent.PostReview)
	model.Summary = stripClassificationTokens(model.Summary)
	for i := range model.Comments {
		model.Comments[i].Body = stripClassificationTokens(model.Comments[i].Body)
		model.Comments[i].Verified = false
		model.Comments[i].Evidence = ""
	}

	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Detail < b.Detail
	})
	comments := make([]intent.ReviewComment, 0, len(findings)+len(model.Comments))
	for _, finding := range findings {
		comments = append(comments, intent.ReviewComment{
			Path:     finding.Path,
			Line:     finding.Line,
			Side:     "RIGHT",
			Body:     finding.Rule + ": " + finding.Detail,
			Verified: true,
			Evidence: finding.Evidence,
		})
	}
	comments = append(comments, model.Comments...)
	if limit := grant.Limits.MaxComments; limit >= 0 && len(comments) > limit {
		comments = comments[:limit]
	}

	trusted := intent.PostReview{Verdict: model.Verdict, Summary: model.Summary, Comments: comments}
	payload, err := json.Marshal(trusted)
	if err != nil {
		return fail(fmt.Errorf("encode trusted review: %w", err))
	}
	envelope := intent.Envelope{
		SchemaVersion: intent.SchemaVersion,
		RunID:         runID,
		Kind:          intent.KindPostReview,
		Payload:       payload,
	}
	execution, err := p.Exec.Execute(ctx, envelope, grant, sandbox.Worktree())
	if err != nil {
		return fail(fmt.Errorf("execute: %w", err))
	}
	if err := p.Store.SetRunRef(ctx, runID, execution.GitHubRef); err != nil {
		return fail(fmt.Errorf("record ref: %w", err))
	}
	if err := p.Store.SetRunStatus(ctx, runID, ledger.RunSucceeded); err != nil {
		return "", err
	}
	return execution.GitHubRef, nil
}

func verifyReviewWorktree(ctx context.Context, worktree, expectedHead string) error {
	head, err := exec.CommandContext(ctx, "git", "-C", worktree, "rev-parse", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("verify review HEAD: %w", err)
	}
	if got := strings.TrimSpace(string(head)); got != expectedHead {
		return fmt.Errorf("read-only contract: HEAD changed from %s to %s", expectedHead, got)
	}
	status, err := exec.CommandContext(ctx, "git", "-C", worktree, "status", "--porcelain").Output()
	if err != nil {
		return fmt.Errorf("verify review worktree: %w", err)
	}
	if len(status) != 0 {
		return fmt.Errorf("read-only contract: worktree modified: %s", strings.TrimSpace(string(status)))
	}
	return nil
}

func stripClassificationTokens(value string) string {
	return classificationToken.ReplaceAllString(value, "")
}
