// Package runtime defines the narrow seam between the maintainer bot and an
// agent backend (Pi SDK, OpenHands agent-server, Claude Agent SDK, ...).
//
// Two properties are structural, not conventional:
//
//  1. The bot does not know which runtime it runs on, and the runtime does not
//     know it is serving a maintainer bot.
//  2. A runtime never sees a Grant or a credential. RunInput carries the task,
//     the sandbox handle, and limits — nothing that answers "where does this
//     land". The agent emits content-only intents; the executor supplies all
//     targeting later.
package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shambu2k/bothos/internal/intent"
)

// Sandbox is the ephemeral per-run container handle. The worker creates it,
// the runtime executes inside it, and the executor later reads the worktree to
// compute the diff. No GitHub token is ever forwarded into it.
type Sandbox interface {
	Exec(ctx context.Context, cmd string, args ...string) (Output, error)
	// Worktree is a path inside the sandbox root that the executor resolves
	// against when computing a diff.
	Worktree() string
}

type Output struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Limits are enforced by the runtime, per the plan's controls (per-run token
// and wall-clock caps).
type Limits struct {
	MaxTokens  int
	MaxSeconds time.Duration
}

// ClaimedFix is one dependency fix the agent claims it made, so an external
// verifier can re-scan and grade it deterministically (the agent cannot grade
// its own homework). AdvisoryID "" means the claim covers any advisory for
// Package.
type ClaimedFix struct {
	Package    string `json:"package"`
	AdvisoryID string `json:"advisory_id"` // "" = match any advisory for the package
	To         string `json:"to"`
}

// Verdict is the agent's structured end-of-run report, written by the agent
// itself. It is prose for the PR body — never used for targeting or gating.
// Fixes names the changes the agent believes it made; the external verifier
// re-scans to confirm them.
type Verdict struct {
	Status       string       `json:"status"`       // one of the Verdict* constants
	Summary      string       `json:"summary"`      // what changed / why blocked
	Verification string       `json:"verification"` // what the agent ran and the result, or why it couldn't verify
	Fixes        []ClaimedFix `json:"fixes,omitempty"`
}

const (
	VerdictDone           = "done"            // change complete, agent verified it itself
	VerdictDoneUnverified = "done_unverified" // change complete, agent could not verify (pre-existing/environmental)
	VerdictBlocked        = "blocked"         // agent cannot complete the change
)

// SecurityTask is the structured input for a security-remediation run. The
// agent does discovery itself (osv-scanner/trivy); the bot supplies no
// candidate. BaseRef is a hint only — the clone's origin/HEAD is truth.
type SecurityTask struct{ BaseRef string }

// RunInput is everything a runtime gets. Note what is absent: no Grant, no
// token, no repo, no scope.
type RunInput struct {
	RunID    string
	Task     SecurityTask
	GraphKey string // graph cache key for codebase context; "" = proceed without
	Sandbox  Sandbox
	Limits   Limits
}

type RunResult struct {
	Intents   []intent.Envelope
	TokensIn  int
	TokensOut int
	CostUSD   float64
	Verdict   *Verdict // nil = agent never produced a valid verdict
}

// AgentRuntime is the swappable agent backend. Implementations: Pi SDK,
// OpenHands agent-server, Claude Agent SDK. Whichever wins on Phase 2's
// upgrade-PR success rate becomes the default.
type AgentRuntime interface {
	Run(ctx context.Context, in RunInput) (RunResult, error)
}

// SecurityPrompt renders the structured task for a security-remediation run.
// The agent owns discovery, triage, branch, fix, commit, and self-verification;
// the bot owns policy. The runID is interpolated so the branch the agent
// creates is stable across feedback rounds.
func SecurityPrompt(runID string, t SecurityTask) string {
	var b strings.Builder

	fmt.Fprintf(&b, "You are a security-remediation agent working in a fresh `--depth 1` clone of this repository's default branch.\n\n")

	fmt.Fprintf(&b, "Your job is to find and fix security vulnerabilities in the dependency tree, and to verify your own work.\n\n")

	fmt.Fprintf(&b, "1. Discover vulnerabilities yourself:\n")
	fmt.Fprintf(&b, "   - Run `osv-scanner --format json .`. Note that an exit code of 1 means findings were found — that is success, not failure.\n")
	fmt.Fprintf(&b, "   - Optionally also run `trivy fs --format json .` for a second read.\n\n")

	fmt.Fprintf(&b, "2. Triage before fixing:\n")
	fmt.Fprintf(&b, "   - Prefer findings that have a fixed version available.\n")
	fmt.Fprintf(&b, "   - Weight direct vs dev dependencies and whether the vulnerable path is actually reachable.\n")
	fmt.Fprintf(&b, "   - Fix a coherent, defensible set — not every last finding.\n\n")

	fmt.Fprintf(&b, "3. Git contract (hard rules):\n")
	fmt.Fprintf(&b, "   - Create the branch `bot/%s-<short-topic-slug>` and commit all your work there.\n", runID)
	fmt.Fprintf(&b, "   - The sandbox has already configured your git identity (user.name/user.email) — commit with that, no `-c` flags needed.\n")
	fmt.Fprintf(&b, "   - NEVER push: no credentials exist here, and pushing is the harness's job.\n")
	fmt.Fprintf(&b, "   - NEVER amend, rebase, or otherwise rewrite the base branch.\n\n")

	fmt.Fprintf(&b, "4. Verify your own work:\n")
	fmt.Fprintf(&b, "   - Run the build/tests you judge appropriate.\n")
	fmt.Fprintf(&b, "   - Re-run `osv-scanner` to confirm the findings are gone.\n")
	fmt.Fprintf(&b, "   - An external verifier will re-check your changes and may send its findings back for another round.\n")

	if t.BaseRef != "" {
		fmt.Fprintf(&b, "\nThe base branch is believed to be %q; confirm it with `git rev-parse --abbrev-ref origin/HEAD`.\n", t.BaseRef)
	}

	return b.String()
}
