// Package scanjob ties a periodic repo scan together: clone -> run scanners ->
// upsert findings. Dependencies are injected so the logic is testable without
// git, network, or the scanner binaries.
package scanjob

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/shambu2k/bothos/internal/scan"
)

// Config carries the injectable pieces of a scan run.
type Config struct {
	// Clone fetches repo (owner/name) into dir. Nil means "no clone" (used by
	// tests that scan a local fixture). Defaults to ShallowClone.
	Clone func(ctx context.Context, dir, repo string) error
	// Tools are the scanners to run against the cloned tree. Defaults to
	// scan.StandardTools().
	Tools []scan.Tool
}

// Upserter persists findings to the ledger.
type Upserter interface {
	UpsertFindings(ctx context.Context, runID string, findings []scan.Finding) error
}

// Run clones repo into a temp dir, runs the scanners, and persists the findings
// against an existing run (the caller inserts the run row so findings stay
// traceable). It returns the number of findings.
func Run(ctx context.Context, cfg Config, store Upserter, repo, runID string) (int, error) {
	clone := cfg.Clone
	if clone == nil {
		clone = ShallowClone
	}
	tools := cfg.Tools
	if tools == nil {
		tools = scan.StandardTools()
	}

	dir, err := os.MkdirTemp("", "bothos-scan-")
	if err != nil {
		return 0, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	if err := clone(ctx, dir, repo); err != nil {
		return 0, fmt.Errorf("clone %s: %w", repo, err)
	}
	findings, err := scan.Run(ctx, dir, tools)
	if err != nil {
		return 0, err
	}
	// The parsers don't know the repo; stamp it so findings are attributable.
	for i := range findings {
		findings[i].RepoID = repo
	}
	if err := store.UpsertFindings(ctx, runID, findings); err != nil {
		return 0, fmt.Errorf("upsert findings: %w", err)
	}
	return len(findings), nil
}

// ShallowClone fetches repo (owner/name) with --depth 1 so periodic scans do
// not drag history into the sandbox. When GITHUB_READ_TOKEN is set (a readonly
// fine-grained PAT, gitignored), it authenticates over https so private repos
// can be scanned without giving the worker any write credential.
func ShallowClone(ctx context.Context, dir, repo string) error {
	url := "https://github.com/" + repo + ".git"
	if tok := os.Getenv("GITHUB_READ_TOKEN"); tok != "" {
		url = "https://x-access-token:" + tok + "@github.com/" + repo + ".git"
	}
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "-q", url, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}
