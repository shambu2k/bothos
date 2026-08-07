// Package runpipe is the worker's deterministic orchestration of an upgrade
// run: load the run + grant, create a sandbox (work branch off the default),
// run the agent runtime, re-verify tests, commit locally, then hand the intent
// to the executor (the only writer). Every dependency is injected so the flow
// is unit-testable without network, git, or a model.
package runpipe

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/shambu2k/bothos/internal/executor"
	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/runtime"
	"github.com/shambu2k/bothos/internal/upgrade"
)

// UpgradeMeta aliases the run-meta payload defined in internal/upgrade.
type UpgradeMeta = upgrade.UpgradeMeta

// Store is what runpipe needs from the ledger for one run.
type Store interface {
	RunByID(ctx context.Context, id string) (ledger.Run, error)
	SetRunStatus(ctx context.Context, id string, s ledger.RunStatus) error
}

// Executor is the write seam (the real one is executor.Executor).
type Executor interface {
	Execute(ctx context.Context, env intent.Envelope, g intent.Grant) (executor.Result, error)
}

// Sandboxer clones repo's default branch, creates work branch, and returns a
// sandbox whose Worktree() is the checkout. Implementations can borrow the
// scanjob clone + a git branch checkout; tests inject a stub.
type Sandboxer func(ctx context.Context, repo, branch, baseRef string) (runtime.Sandbox, error)

// Pipeline orchestrates one upgrade run.
type Pipeline struct {
	Store   Store
	Agent   runtime.AgentRuntime
	Exec    Executor
	Sandbox Sandboxer
	// Commit stages and commits the worktree on branch (local, no token).
	Commit func(ctx context.Context, worktree, branch string) error
}

// Run executes the upgrade orchestration and returns the GitHub ref (e.g.
// "owner/repo#123") on success. It marks the run running/failed/succeeded.
func (p *Pipeline) Run(ctx context.Context, runID string) (string, error) {
	run, err := p.Store.RunByID(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("load run: %w", err)
	}
	var g intent.Grant
	if err := json.Unmarshal(run.Grant, &g); err != nil {
		return "", fmt.Errorf("grant: %w", err)
	}
	var m UpgradeMeta
	if err := json.Unmarshal(run.Meta, &m); err != nil {
		return "", fmt.Errorf("meta: %w", err)
	}
	if m.Package == "" || m.To == "" {
		return "", fmt.Errorf("upgrade run missing meta (pkg/to)")
	}

	fail := func(err error) (string, error) {
		_ = p.Store.SetRunStatus(ctx, runID, ledger.RunFailed)
		return "", err
	}
	if err := p.Store.SetRunStatus(ctx, runID, ledger.RunRunning); err != nil {
		return "", err
	}

	topic := "upgrade-" + m.Package + "-" + m.To
	branch := "bot/" + runID + "-" + topic
	sb, err := p.Sandbox(ctx, g.Repo.Owner+"/"+g.Repo.Name, branch, g.Scope.BaseRef)
	if err != nil {
		return fail(fmt.Errorf("sandbox: %w", err))
	}

	task := runtime.UpgradeTask{Package: m.Package, CurrentVersion: m.From, TargetVersion: m.To}
	if task.TestCommand == "" {
		task.TestCommand = upgrade.TestCommandFor(sb.Worktree())
	}

	// A sane wall-clock cap (the grant's intent.Limits carries no duration).
	// JIVA is a large backend: clone + npm install + tests + edits routinely
	// exceed 15m, so cap generously at 40m (drafts are cheap to kill).
	wall := 40 * time.Minute
	res, err := p.Agent.Run(ctx, runtime.RunInput{
		RunID:   runID,
		Task:    task,
		Sandbox: sb,
		Limits:  runtime.Limits{MaxSeconds: wall},
	})
	if err != nil {
		return fail(fmt.Errorf("agent: %w", err))
	}
	verdictStatus := "none"
	if res.Verdict != nil {
		verdictStatus = res.Verdict.Status
	}
	log.Printf("run %s: agent verdict %q", runID, verdictStatus)
	var openPR *intent.Envelope
	for i := range res.Intents {
		if res.Intents[i].Kind == "open_pr" {
			openPR = &res.Intents[i]
			break
		}
	}
	if openPR == nil {
		return fail(fmt.Errorf("no open_pr intent produced"))
	}

	if p.Commit != nil {
		if err := p.Commit(ctx, sb.Worktree(), branch); err != nil {
			return fail(fmt.Errorf("commit: %w", err))
		}
	}

	cr, err := p.Exec.Execute(ctx, *openPR, g)
	if err != nil {
		return fail(fmt.Errorf("execute: %w", err))
	}
	if err := p.Store.SetRunStatus(ctx, runID, ledger.RunSucceeded); err != nil {
		return "", err
	}
	return cr.GitHubRef, nil
}
