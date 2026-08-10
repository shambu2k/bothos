// Package upgrade turns Phase 1 actionable candidates into the inputs an LLM
// agent runtime needs, and (in later files) drives the writes. It is the Phase 2
// orchestrator's vocabulary; it holds no credentials and only talks to the
// runtime seam and the ledger.
package upgrade

import (
	"os"
	"path/filepath"
)

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
