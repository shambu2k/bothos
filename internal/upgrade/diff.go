package upgrade

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/shambu2k/bothos/internal/intent"
)

// GitDiff implements the executor's DiffSource over a real git worktree: it
// diffs the worktree against the base ref (the repo default branch) and returns
// the file list + added/deleted line counts that the grant's ValidateDiff
// enforces. The executor constructs this per-run with the grant's BaseRef.
type GitDiff struct{ Base string }

func (g GitDiff) FromWorktree(ctx context.Context, runID, worktree string) (intent.Diff, error) {
	if g.Base == "" {
		return intent.Diff{}, fmt.Errorf("git diff: no base ref")
	}
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "diff", "--numstat", g.Base)
	out, err := cmd.Output()
	if err != nil {
		return intent.Diff{}, fmt.Errorf("git diff: %w", err)
	}
	var d intent.Diff
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue // binary lines are "- - path"
		}
		d.Files = append(d.Files, parts[2])
		if a, err := strconv.Atoi(parts[0]); err == nil {
			d.AddedLines += a
		}
		if del, err := strconv.Atoi(parts[1]); err == nil {
			d.DeletedLines += del
		}
	}
	return d, nil
}
