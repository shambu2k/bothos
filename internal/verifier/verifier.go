// Package verifier re-checks, deterministically, the fixes an agent claims to
// have made. The agent cannot grade its own homework; this package re-scans
// with the same osv-scanner the Phase 1 scan uses and checks the worktree is
// committed and tests pass. It is deliberately osv-only (no trivy DB download
// in the verify path — that would make every feedback round slow and flaky).
package verifier

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/shambu2k/bothos/internal/runtime"
	"github.com/shambu2k/bothos/internal/scan"
	"github.com/shambu2k/bothos/internal/upgrade"
)

// Rule constants name the failure classes a verify round can produce.
const (
	RuleUncommitted  = "uncommitted_changes"
	RuleVulnPresent  = "vuln_still_present"
	RuleTestFailed   = "test_failed"
	RuleScannerError = "scanner_error"
)

// Failure is one thing a verify round found wrong.
type Failure struct {
	Rule    string // one of the Rule* constants
	Detail  string // e.g. "tar GHSA-abc still present at 7.5.16"
	Snippet string // bounded output, ≤1024 chars
}

// Result is the outcome of one verify round.
type Result struct {
	Pass     bool
	Failures []Failure
}

// Verifier re-checks an agent's claimed fixes against the worktree.
type Verifier struct {
	Tools       []scan.Tool                  // default scan.StandardTools()
	TestCommand func(worktree string) string // default upgrade.TestCommandFor
	TestTimeout time.Duration                // default 10m
}

// Verify checks, in order: a clean committed worktree, the claimed fixes
// against a deterministic re-scan, and (when a test command is known) the
// test suite. Every failure is fed back to the agent; none of these is a
// hard error — the run becomes a feedback round instead.
func (v Verifier) Verify(ctx context.Context, worktree string, claimed []runtime.ClaimedFix) Result {
	if v.Tools == nil {
		v.Tools = scan.StandardTools()
	}
	tc := v.TestCommand
	if tc == nil {
		tc = upgrade.TestCommandFor
	}
	timeout := v.TestTimeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}

	var fails []Failure

	// 1. Committed? The agent must commit; the harness never commits for it.
	// .bothos/ is harness bookkeeping (the verdict file the agent writes
	// while settling) and is deleted at the end of every run — it must never
	// count as an uncommitted change, or every feedback round would report it.
	if out, err := exec.CommandContext(ctx, "git", "-C", worktree, "status", "--porcelain").Output(); err != nil {
		fails = append(fails, Failure{Rule: RuleScannerError, Detail: "git status: " + err.Error()})
	} else if s := strings.TrimSpace(stripBothos(string(out))); s != "" {
		fails = append(fails, Failure{
			Rule:    RuleUncommitted,
			Detail:  "worktree has uncommitted or untracked changes",
			Snippet: truncSnippet(s, 1024),
		})
	}

	// 2. Re-scan and grade each claimed fix. A scanner exec/parse error is a
	// failure that gets fed back — it is never silently ignored.
	if findings, err := scan.Run(ctx, worktree, v.Tools); err != nil {
		fails = append(fails, Failure{Rule: RuleScannerError, Detail: "scanner: " + err.Error()})
	} else {
		for _, c := range claimed {
			for _, f := range findings {
				if f.Package != c.Package {
					continue
				}
				if c.AdvisoryID != "" && f.AdvisoryID != c.AdvisoryID {
					continue
				}
				ver := f.CurrentVersion
				if ver == "" {
					ver = "unknown"
				}
				id := f.AdvisoryID
				if id == "" {
					id = "(no advisory)"
				}
				fails = append(fails, Failure{
					Rule:   RuleVulnPresent,
					Detail: fmt.Sprintf("%s %s still present at %s", c.Package, id, ver),
				})
				break
			}
		}
	}

	// 3. Tests, when a conventional command is recognisable.
	if cmd := tc(worktree); cmd != "" {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		c := exec.CommandContext(cctx, "sh", "-c", cmd)
		c.Dir = worktree
		out, err := c.CombinedOutput()
		cancel()
		if err != nil {
			fails = append(fails, Failure{
				Rule:    RuleTestFailed,
				Detail:  fmt.Sprintf("%q exited non-zero", cmd),
				Snippet: tailBytes(out, 1024),
			})
		}
	}

	return Result{Pass: len(fails) == 0, Failures: fails}
}

// Signature returns a stable digest of a failure set (sorted rule|detail
// lines). Two identical failure sets produce the same signature — the stall
// detector uses it to stop the feedback loop when the agent stops making
// progress.
func Signature(f []Failure) string {
	lines := make([]string, 0, len(f))
	for _, x := range f {
		lines = append(lines, x.Rule+"|"+x.Detail)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

func truncSnippet(s string, max int) string {
	return truncStr(s, max)
}

// stripBothos removes .bothos/ entries from git status --porcelain output.
// .bothos/ is harness bookkeeping (the verdict file the agent writes while
// settling); it is not part of the agent's change and must not be reported
// as an uncommitted/untracked change.
func stripBothos(porcelain string) string {
	var kept []string
	for _, line := range strings.Split(porcelain, "\n") {
		if strings.Contains(line, ".bothos/") || strings.HasPrefix(line, "?? .bothos/") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func truncStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

// tailBytes returns the last max bytes of out as a string (the test-failure
// snippet convention: the tail carries the actual failure).
func tailBytes(out []byte, max int) string {
	if len(out) <= max {
		return string(out)
	}
	return string(out[len(out)-max:])
}
