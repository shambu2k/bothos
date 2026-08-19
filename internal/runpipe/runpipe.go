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
	SetRunFailure(ctx context.Context, id, reason string) error
	SetRunNeedsInput(ctx context.Context, id, reason string) error
	SetRunRef(ctx context.Context, id, ref string) error
}

// Executor is the write seam (the real one is executor.Executor).
type Executor interface {
	Execute(ctx context.Context, env intent.Envelope, g intent.Grant, worktree string) (executor.Result, error)
}

// Sandboxer clones repo's default branch and returns a sandbox whose
// Worktree() is the checkout. The agent branches and commits inside it; the
// executor later reads both from git state. Implementations can borrow the
// scanjob clone; tests inject a stub.
type Sandboxer func(ctx context.Context, repo string) (runtime.Sandbox, error)

// Pipeline orchestrates one security run.
type Pipeline struct {
	Store   Store
	Agent   runtime.AgentRuntime
	Exec    Executor
	Sandbox Sandboxer
	// Now returns the current time. It defaults to time.Now but may be
	// injected in tests to make grant-expiry checks deterministic.
	Now func() time.Time
}

// now is the injected-clock seam: nil falls back to the wall clock so
// struct-literal assembly in cmd/worker/main.go needs no field.
func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Run executes the security orchestration and returns the GitHub ref (e.g.
// "owner/repo#123") on success. It marks the run running/failed/succeeded.
// A stand-down (agent blocked, no open_pr intent) is terminal: it records the
// failure reason and returns nil — it must NOT become a River retry.
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

	fail := func(err error) (string, error) {
		_ = p.Store.SetRunStatus(ctx, runID, ledger.RunFailed)
		_ = p.Store.SetRunFailure(ctx, runID, err.Error())
		return "", err
	}
	if err := p.Store.SetRunStatus(ctx, runID, ledger.RunRunning); err != nil {
		return "", err
	}

	// A stale grant is a terminal, zero-value run: the executor would reject
	// any intent at execute time and the agent run (up to the wall cap) is
	// pure wasted tokens. Fail fast before starting the agent instead.
	if p.now().After(g.ExpiresAt) {
		return fail(fmt.Errorf("grant expired at %s (dispatched %s)", g.ExpiresAt, g.IssuedAt))
	}

	sb, err := p.Sandbox(ctx, g.Repo.Owner+"/"+g.Repo.Name)
	if err != nil {
		return fail(fmt.Errorf("sandbox: %w", err))
	}

	// A sane wall-clock cap (the grant's intent.Limits carries no duration).
	// Large-backend clones routinely exceed 15m, so cap generously at 40m
	// (drafts are cheap to kill).
	wall := time.Duration(runtime.AgentWallSeconds) * time.Second
	res, err := p.Agent.Run(ctx, runtime.RunInput{
		RunID:   runID,
		Task:    runtime.SecurityTask{BaseRef: m.BaseRef},
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
		// Stand-down: a blocked verdict means the agent could not complete the
		// change and no PR should be opened. This is terminal for the run, not
		// a transient failure — so record the reason and do NOT fail() (which
		// would make River retry).
		if res.Verdict != nil && res.Verdict.Status == runtime.VerdictBlocked {
			if err := p.Store.SetRunFailure(ctx, runID, "agent blocked: "+res.Verdict.Summary); err != nil {
				return "", fmt.Errorf("record failure: %w", err)
			}
			log.Printf("run %s: agent stood down (blocked)", runID)
			return "", nil
		}
		return fail(fmt.Errorf("no open_pr intent produced"))
	}

	cr, err := p.Exec.Execute(ctx, *openPR, g, sb.Worktree())
	if err != nil {
		return fail(fmt.Errorf("execute: %w", err))
	}
	if err := p.Store.SetRunRef(ctx, runID, cr.GitHubRef); err != nil {
		return "", fmt.Errorf("record ref: %w", err)
	}
	if err := p.Store.SetRunStatus(ctx, runID, ledger.RunSucceeded); err != nil {
		return "", err
	}
	return cr.GitHubRef, nil
}
