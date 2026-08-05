// Package upgrade turns Phase 1 actionable candidates into the inputs an LLM
// agent runtime needs, and (in later files) drives the writes. It is the Phase 2
// orchestrator's vocabulary; it holds no credentials and only talks to the
// runtime seam and the ledger.
package upgrade

import (
	"os"
	"path/filepath"

	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/runtime"
)

// UpgradeTaskFromCandidate maps a candidate (a finding with a fix) to the
// structured task the agent runtime consumes. testCmd is a best-effort hint the
// agent may refine; the worker re-verifies the real result before any PR.
func UpgradeTaskFromCandidate(c ledger.Candidate, testCmd string) runtime.UpgradeTask {
	return runtime.UpgradeTask{
		Package:        c.Package,
		CurrentVersion: c.CurrentVersion,
		TargetVersion:  c.TargetVersion,
		TestCommand:    testCmd,
	}
}

// TestCommandFor detects a repo's conventional test command from its manifest.
// Returns "" when nothing recognisable is present (the agent then decides).
func TestCommandFor(worktree string) string {
	if fileExists(filepath.Join(worktree, "go.mod")) {
		return "go test ./..."
	}
	if fileExists(filepath.Join(worktree, "package.json")) {
		return "npm test"
	}
	return ""
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
