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
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Command  string `json:"command,omitempty"`
	Success  *bool  `json:"success,omitempty"`
	ToolName string `json:"toolName,omitempty"`
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
	if err := enc.Encode(map[string]any{
		"id":      "1",
		"type":    "prompt",
		"message": runtime.UpgradePrompt(in.Task),
	}); err != nil {
		_ = cmd.Process.Kill()
		return runtime.RunResult{}, fmt.Errorf("write prompt: %w (%s)", err, errBuf.String())
	}

	// Stream events until the agent turn completes.
	rejected := false
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		var ev rpcEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue // diagnostics / non-JSON line: not fatal
		}
		switch ev.Type {
		case "tool_execution_start":
			log.Printf("run %s: RPC tool %q", in.RunID, ev.ToolName)
		case "response":
			if ev.Command == "prompt" && ev.Success != nil && !*ev.Success {
				rejected = true
			}
		case "agent_end":
			goto done // agent turn finished
		}
	}
done:
	// Close stdin → PI's RPC loop shuts down gracefully, persisting the session.
	_ = stdin.Close()

	if err := cmd.Wait(); err != nil {
		// Distinguish our cancellation from an external kill (research: Go's
		// "signal: killed" is ambiguous; log the context cause + elapsed time).
		log.Printf("run %s: pi rpc exited: %v (ctx=%v elapsed=%s stderr=%s)",
			in.RunID, err, ctx.Err(), time.Since(start).Round(time.Millisecond), errBuf.String())
		return runtime.RunResult{}, fmt.Errorf("pi rpc: %w (ctx=%v)", err, ctx.Err())
	}
	if rejected {
		return runtime.RunResult{}, fmt.Errorf("pi rpc: prompt rejected")
	}

	// Diff gate: the agent must have actually changed the worktree.
	if !worktreeChanged(worktree) {
		return runtime.RunResult{}, fmt.Errorf("agent made no changes to the worktree")
	}

	return runtime.RunResult{Intents: []intent.Envelope{openPRIntent(in.RunID, worktree, in.Task)}}, nil
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
// the executor from the Grant — the agent contributes content only.
func openPRIntent(runID, worktree string, t runtime.UpgradeTask) intent.Envelope {
	pc := intent.OpenPR{
		Title:    fmt.Sprintf("chore(deps): upgrade %s to %s (security)", t.Package, t.TargetVersion),
		Body:     fmt.Sprintf("Security dependency upgrade: %s %s -> %s.", t.Package, t.CurrentVersion, t.TargetVersion),
		Draft:    true,
		Worktree: worktree,
		Topic:    fmt.Sprintf("upgrade-%s-%s", t.Package, t.TargetVersion),
	}
	raw, _ := json.Marshal(pc)
	return intent.Envelope{SchemaVersion: intent.SchemaVersion, RunID: runID, Kind: intent.KindOpenPR, Payload: raw}
}

var _ runtime.AgentRuntime = (*RPC)(nil)
