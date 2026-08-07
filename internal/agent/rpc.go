// Package agent hosts the integration with an agent runtime behind a process
// boundary. The first and canonical backend is PI Agent's RPC mode
// (`pi --mode rpc`): a documented, long-lived, session-persistent protocol
// over stdin/stdout JSONL. This replaces the earlier bespoke Node SDK sidecar,
// which SIGKILL'd its child on any context cancellation and left the worker
// without proper init supervision.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/runtime"
)

// defaultModel is the OpenRouter model used unless the runtime is configured
// otherwise (PI_MODEL env / registry config). It matches the Phase 2 choice.
const defaultModel = "openrouter/deepseek/deepseek-v4-flash-0731"

// RPC drives the PI CLI in --mode rpc. It implements runtime.AgentRuntime.
type RPC struct {
	piBin      string        // "pi" executable
	model      string        // provider/id ("" => defaultModel)
	sessionDir string        // root for per-run --session-dir (persistent)
	approve    bool          // pass --approve (trust throwaway sandbox)
	WaitDelay  time.Duration // backstop bound after SIGTERM (default 15s)
}

// NewPIRPC returns an RPC configured from zero or a partially-filled model.
// Empty fields fall back to defaults; the worker overrides via registry cfg.
func NewPIRPC(piBin, model, sessionDir string, approve bool) *RPC {
	if piBin == "" {
		piBin = "pi"
	}
	if model == "" {
		model = defaultModel
	}
	return &RPC{piBin: piBin, model: model, sessionDir: sessionDir, approve: approve, WaitDelay: 15 * time.Second}
}

// rpcEvent is the subset of PI RPC stdout events we consume. Unknown fields are
// ignored; unknown event types are skipped, never fatal.
type rpcEvent struct {
	ID        string `json:"id,omitempty"`
	Type      string `json:"type,omitempty"`
	Command   string `json:"command,omitempty"`
	Success   *bool  `json:"success,omitempty"`
	ToolName  string `json:"toolName,omitempty"`
	WillRetry bool   `json:"willRetry,omitempty"`
}

// Run runs one upgrade task through a per-run `pi --mode rpc` subprocess whose
// cwd is the sandbox worktree and whose session is persisted under
// sessionDir/<runID>. After the agent finishes it gates on a real worktree diff
// and, if there is one, returns a deterministic open_pr intent.
func (r *RPC) Run(ctx context.Context, in runtime.RunInput) (runtime.RunResult, error) {
	worktree := in.Sandbox.Worktree()

	// Persistent, per-run session directory.
	dir := filepath.Join(r.sessionDir, in.RunID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return runtime.RunResult{}, fmt.Errorf("session dir: %w", err)
	}

	// Wall-clock backstop (the agent is allowed a long, interruptible run).
	if in.Limits.MaxSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, in.Limits.MaxSeconds)
		defer cancel()
	}

	args := []string{"--mode", "rpc", "--session-dir", dir, "--model", r.model}
	if r.approve {
		args = append(args, "--approve")
	}

	cmd := exec.CommandContext(ctx, r.piBin, args...)
	cmd.Dir = worktree
	// Own process group: the child (and its tools) won't be swept by signals
	// sent to the worker's group, and we can target it precisely on shutdown.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = os.Environ()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return runtime.RunResult{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runtime.RunResult{}, err
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf

	// Graceful shutdown: on context cancellation send SIGTERM to the process
	// group (PI shuts down cleanly and persists); WaitDelay bounds a stuck
	// child before Go escalates to SIGKILL. Never SIGKILL on first notice.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		return nil
	}
	cmd.WaitDelay = r.WaitDelay

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return runtime.RunResult{}, fmt.Errorf("start pi rpc: %w", err)
	}

	enc := json.NewEncoder(stdin)
	// The initial prompt carries the structured task plus the verdict contract.
	if err := enc.Encode(map[string]any{
		"id":      "1",
		"type":    "prompt",
		"message": runtime.UpgradePrompt(in.Task) + reportingInstructions,
	}); err != nil {
		_ = cmd.Process.Kill()
		return runtime.RunResult{}, fmt.Errorf("write prompt: %w (%s)", err, errBuf.String())
	}

	sc := bufio.NewScanner(stdout)

	// Settle -> verdict -> optional single nudge. The loop stays open across a
	// settle so the agent can be steered once if it omitted the verdict.
	settleErr := r.awaitSettled(in.RunID, sc)
	var v *runtime.Verdict
	if settleErr == nil {
		v = readVerdict(worktree)
		if v == nil {
			// Exactly one nudge, then we re-read once and move on regardless.
			if err := enc.Encode(map[string]any{
				"id":      "2",
				"type":    "prompt",
				"message": "You did not write .bothos/verdict.json (or it was invalid JSON, or its \"status\" was not one of done / done_unverified / blocked). Write it now, exactly as specified, then stop.",
			}); err == nil {
				_ = r.awaitSettled(in.RunID, sc) // second settle may fail; verdict re-read below governs
				v = readVerdict(worktree)
			}
		}
	}

	// The .bothos directory is harness bookkeeping; it must never reach the
	// diff gate or the PR.
	_ = os.RemoveAll(filepath.Join(worktree, ".bothos"))

	// Close stdin → PI's RPC loop shuts down gracefully, persisting the session.
	_ = stdin.Close()

	if err := cmd.Wait(); err != nil {
		// Distinguish our cancellation from an external kill (research: Go's
		// "signal: killed" is ambiguous; log the context cause + elapsed time).
		log.Printf("run %s: pi rpc exited: %v (ctx=%v elapsed=%s stderr=%s)",
			in.RunID, err, ctx.Err(), time.Since(start).Round(time.Millisecond), errBuf.String())
		return runtime.RunResult{}, fmt.Errorf("pi rpc: %w (ctx=%v)", err, ctx.Err())
	}
	if settleErr != nil {
		// Prompt rejection, or the process exited before settling cleanly.
		return runtime.RunResult{}, settleErr
	}

	// Diff gate: the agent must have actually changed the worktree.
	if !worktreeChanged(worktree) {
		return runtime.RunResult{}, fmt.Errorf("agent made no changes to the worktree")
	}

	return runtime.RunResult{
		Intents: []intent.Envelope{openPRIntent(in.RunID, worktree, in.Task, v)},
		Verdict: v,
	}, nil
}

// reportingInstructions is appended to the initial prompt so the agent knows
// how to report its outcome. The verdict is prose for the PR body — never used
// for targeting or gating. It belongs to this harness, not the runtime seam.
const reportingInstructions = `

When you finish, you MUST report your outcome by writing the file
.bothos/verdict.json in the repository root with this exact shape:

{"status": "done" | "done_unverified" | "blocked", "summary": "...", "verification": "..."}

- "done": the change is complete and you verified it yourself (you ran the
  build/tests/checks you judged appropriate, and they passed). In
  "verification", state exactly what you ran and the result.
- "done_unverified": the change is complete but you could not verify it (for
  example: tests fail for pre-existing or environmental reasons unrelated to
  your change, or no usable test setup exists). In "verification", state what
  you tried and why it is inconclusive.
- "blocked": you cannot complete the change. In "summary", explain why.

How you validate is your decision: the full test suite, targeted tests, a
build, or nothing — whatever fits this change. If a check fails, determine
whether YOUR change caused it: fix what you broke, and do not chase
pre-existing or environmental failures. The .bothos directory is harness
bookkeeping; it is deleted when your run ends and must not be part of the
change itself.`

// awaitSettled scans RPC events until the agent is fully settled: an
// agent_settled event, or agent_end with willRetry=false (fallback for pi
// builds that predate agent_settled — we never queue steer/follow-up
// messages, so the two are equivalent here). A prompt rejection (response
// with command=prompt, success=false) and EOF before settling are errors.
func (r *RPC) awaitSettled(runID string, sc *bufio.Scanner) error {
	for sc.Scan() {
		var ev rpcEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue // diagnostics / non-JSON line: not fatal
		}
		switch ev.Type {
		case "tool_execution_start":
			log.Printf("run %s: RPC tool %q", runID, ev.ToolName)
		case "response":
			if ev.Command == "prompt" && ev.Success != nil && !*ev.Success {
				return fmt.Errorf("pi rpc: prompt rejected")
			}
		case "agent_settled":
			return nil
		case "agent_end":
			if !ev.WillRetry {
				return nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("pi rpc: scan: %w", err)
	}
	return fmt.Errorf("pi rpc: agent did not settle (EOF)")
}

// readVerdict parses <worktree>/.bothos/verdict.json. Returns nil, nil when
// absent, unparsable, or carrying an unknown status — callers treat all three
// as "no verdict".
func readVerdict(worktree string) *runtime.Verdict {
	b, err := os.ReadFile(filepath.Join(worktree, ".bothos", "verdict.json"))
	if err != nil {
		return nil
	}
	var v runtime.Verdict
	if err := json.Unmarshal(b, &v); err != nil {
		return nil
	}
	switch v.Status {
	case runtime.VerdictDone, runtime.VerdictDoneUnverified, runtime.VerdictBlocked:
		return &v
	default:
		return nil
	}
}

// worktreeChanged reports whether the worktree differs from a clean baseline,
// including new (untracked) files such as a freshly generated lockfile.
func worktreeChanged(worktree string) bool {
	out, err := exec.Command("git", "-C", worktree, "status", "--porcelain").Output()
	if err != nil {
		return false // not a repo / git error ⇒ treat as unchanged
	}
	return len(bytes.TrimSpace(out)) > 0
}

// openPRIntent builds the single deterministic open_pr intent the runpipe and
// executor already expect. Targeting (branch, base, repo) is resolved later by
// the executor from the Grant — the agent contributes content only. The
// agent's verdict, when present, is appended to the PR body as prose.
func openPRIntent(runID, worktree string, t runtime.UpgradeTask, v *runtime.Verdict) intent.Envelope {
	body := fmt.Sprintf("Security dependency upgrade: %s %s -> %s.", t.Package, t.CurrentVersion, t.TargetVersion)

	heading, summary, verification := "", "", ""
	switch {
	case v == nil:
		heading = "**Agent report (unverified change — review carefully):**"
		summary, verification = "the agent did not report a run status", "the agent did not report a run status"
	case v.Status == runtime.VerdictDone:
		heading = "**Agent report:**"
		summary, verification = v.Summary, v.Verification
	case v.Status == runtime.VerdictDoneUnverified:
		heading = "**Agent report (unverified change — review carefully):**"
		summary, verification = v.Summary, v.Verification
	case v.Status == runtime.VerdictBlocked:
		heading = "**Agent report (BLOCKED — change incomplete):**"
		summary, verification = v.Summary, v.Verification
	}
	if heading != "" {
		body = fmt.Sprintf("%s\n\n---\n%s %s\n\n**Verification:** %s", body, heading, summary, verification)
	}

	pc := intent.OpenPR{
		Title:    fmt.Sprintf("chore(deps): upgrade %s to %s (security)", t.Package, t.TargetVersion),
		Body:     body,
		Draft:    true,
		Worktree: worktree,
		Topic:    fmt.Sprintf("upgrade-%s-%s", t.Package, t.TargetVersion),
	}
	raw, _ := json.Marshal(pc)
	return intent.Envelope{SchemaVersion: intent.SchemaVersion, RunID: runID, Kind: intent.KindOpenPR, Payload: raw}
}

var _ runtime.AgentRuntime = (*RPC)(nil)
