package runpipe

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/runtime"
)

// IssuePipeline orchestrates an already-authorized labeled-issue run. The
// persisted issue title/body are untrusted context only; repository, issue
// number, branch base, and all write capabilities come from the pre-agent
// grant. A blocked verdict produces a pipeline-owned issue handoff and a
// terminal needs_input status. Reply/resume behavior is intentionally separate.
type IssuePipeline struct {
	Store   Store
	Agent   runtime.AgentRuntime
	Exec    Executor
	Sandbox Sandboxer
}

func (p *IssuePipeline) Run(ctx context.Context, runID string) (string, error) {
	run, err := p.Store.RunByID(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("load run: %w", err)
	}
	var grant intent.Grant
	if err := json.Unmarshal(run.Grant, &grant); err != nil {
		return "", fmt.Errorf("grant: %w", err)
	}
	var stored runtime.IssueTask
	if err := json.Unmarshal(run.Meta, &stored); err != nil {
		return "", fmt.Errorf("issue metadata: %w", err)
	}

	fail := func(cause error) (string, error) {
		_ = p.Store.SetRunStatus(ctx, runID, ledger.RunFailed)
		_ = p.Store.SetRunFailure(ctx, runID, cause.Error())
		return "", cause
	}
	if err := p.Store.SetRunStatus(ctx, runID, ledger.RunRunning); err != nil {
		return "", err
	}
	if grant.Scope.Kind != intent.ScopeIssue || grant.Scope.Number < 1 {
		return fail(fmt.Errorf("issue pipeline requires a numbered issue grant"))
	}
	if time.Now().After(grant.ExpiresAt) {
		return fail(fmt.Errorf("grant expired at %s (dispatched %s)", grant.ExpiresAt, grant.IssuedAt))
	}

	// Never permit persisted metadata to target the agent at another resource.
	// Title/body are intentionally the only fields retained from the webhook.
	task := runtime.IssueTask{
		IssueNumber: grant.Scope.Number,
		Title:       stored.Title,
		Body:        stored.Body,
		RepoURL:     "https://github.com/" + grant.Repo.Owner + "/" + grant.Repo.Name + ".git",
		BaseRef:     grant.Scope.BaseRef,
	}
	sb, err := p.Sandbox(ctx, grant.Repo.Owner+"/"+grant.Repo.Name)
	if err != nil {
		return fail(fmt.Errorf("sandbox: %w", err))
	}

	res, err := p.Agent.Run(ctx, runtime.RunInput{
		RunID: runID, Task: task, Sandbox: sb,
		Limits: runtime.Limits{MaxSeconds: time.Duration(runtime.AgentWallSeconds) * time.Second},
	})
	if err != nil {
		return fail(fmt.Errorf("agent: %w", err))
	}

	if res.Verdict != nil && res.Verdict.Status == runtime.VerdictBlocked {
		return p.recordBlocked(ctx, runID, grant, sb.Worktree(), res.Verdict)
	}
	if len(res.Intents) != 1 || res.Intents[0].Kind != intent.KindOpenPR {
		return fail(fmt.Errorf("issue run must produce exactly one open_pr intent"))
	}
	var proposal intent.OpenPR
	if err := json.Unmarshal(res.Intents[0].Payload, &proposal); err != nil {
		return fail(fmt.Errorf("decode open_pr: %w", err))
	}
	if !proposal.Draft {
		return fail(fmt.Errorf("issue run open_pr must be a draft"))
	}

	result, err := p.Exec.Execute(ctx, res.Intents[0], grant, sb.Worktree())
	if err != nil {
		return fail(fmt.Errorf("execute: %w", err))
	}
	if err := p.Store.SetRunRef(ctx, runID, result.GitHubRef); err != nil {
		return "", fmt.Errorf("record ref: %w", err)
	}
	if err := p.Store.SetRunStatus(ctx, runID, ledger.RunSucceeded); err != nil {
		return "", err
	}
	return result.GitHubRef, nil
}

func (p *IssuePipeline) recordBlocked(ctx context.Context, runID string, grant intent.Grant, worktree string, verdict *runtime.Verdict) (string, error) {
	summary := strings.TrimSpace(verdict.Summary)
	if summary == "" {
		summary = "The agent could not identify a safe complete change."
	}
	body := fmt.Sprintf("**Bothos is blocked.**\n\n%s\n\nNo pull request was opened. Please reply with one answer that resolves the blocker, then trigger a new labeled run.", summary)
	payload, err := json.Marshal(intent.PostComment{Body: body})
	if err != nil {
		return "", fmt.Errorf("encode handoff: %w", err)
	}
	env := intent.Envelope{
		SchemaVersion: intent.SchemaVersion,
		RunID:         runID,
		Kind:          intent.KindPostComment,
		Payload:       payload,
	}
	result, err := p.Exec.Execute(ctx, env, grant, worktree)
	if err != nil {
		return "", fmt.Errorf("post blocked handoff: %w", err)
	}
	if err := p.Store.SetRunRef(ctx, runID, result.GitHubRef); err != nil {
		return "", fmt.Errorf("record handoff ref: %w", err)
	}
	if err := p.Store.SetRunNeedsInput(ctx, runID, "agent blocked: "+summary); err != nil {
		return "", fmt.Errorf("record blocked state: %w", err)
	}
	return result.GitHubRef, nil
}
