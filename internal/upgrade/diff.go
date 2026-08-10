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
// diffs the worktree against origin/HEAD and returns the file list +
// added/deleted line counts that the grant's ValidateDiff enforces. The base
// comes from git state (origin/HEAD), never from a value transported through
// the envelope — that kept the branch/base/topic seam bugs possible.
type GitDiff struct{}

func (GitDiff) FromWorktree(ctx context.Context, runID, worktree string) (intent.Diff, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "diff", "--numstat", "origin/HEAD")
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

// BaseBranch returns the repo's default branch short name (e.g. "main")
// resolved from the clone's origin/HEAD — git state is the single source.
func BaseBranch(ctx context.Context, worktree string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", worktree, "rev-parse", "--abbrev-ref", "origin/HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("resolve origin/HEAD: %w", err)
	}
	name := strings.TrimSpace(string(out))
	name = strings.TrimPrefix(name, "origin/")
	if name == "" || name == "HEAD" {
		return "", fmt.Errorf("unresolved default branch %q", name)
	}
	return name, nil
}

// CurrentBranch returns the checked-out branch (git rev-parse --abbrev-ref HEAD).
func CurrentBranch(ctx context.Context, worktree string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", worktree, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("resolve HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
