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
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/runtime"
	"github.com/shambu2k/bothos/internal/verifier"
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

	// Verify re-checks the agent's claimed fixes deterministically (defaults to
	// a verifier.Verifier when nil) and MaxRounds bounds the feedback loop.
	// Inject both in tests to script verifier trajectories.
	Verify    func(ctx context.Context, worktree string, fixes []runtime.ClaimedFix) (verifier.Result, error)
	MaxRounds int

	// Logger, when non-nil, receives structured run logs (each with a
	// run_id attribute); nil falls back to slog.Default().
	Logger *slog.Logger
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

// runLogger returns a logger carrying runID as a structured run_id field,
// falling back to slog.Default() when no logger was injected.
func (r *RPC) runLogger(runID string) *slog.Logger {
	if r.Logger != nil {
		return r.Logger.With("run_id", runID)
	}
	return slog.Default().With("run_id", runID)
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
	var initialPrompt string
	reviewMode := false
	issueMode := false
	verifyMode := false
	switch task := in.Task.(type) {
	case runtime.SecurityTask:
		initialPrompt = runtime.SecurityPrompt(in.RunID, task) + reportingInstructions
		verifyMode = true
	case runtime.IssueTask:
		initialPrompt = runtime.IssuePrompt(in.RunID, task) + reportingInstructions
		issueMode = true
	case runtime.ReviewTask:
		initialPrompt = runtime.ReviewPrompt(task) + reviewReportingInstructions
		reviewMode = true
	default:
		return runtime.RunResult{}, fmt.Errorf("unsupported task type %T", in.Task)
	}

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
	// The pi subprocess must never inherit the executor's write token: strip
	// every GITHUB_WRITE_TOKEN* variable (global and per-account) before fork.
	cmd.Env = withoutSecrets(os.Environ())

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

	// A single shared scanner for the whole lifecycle. PI can emit very long
	// single lines (large diffs, verbose tool output, JSON blobs); the bufio
	// default max token is 64KB and will abort with "token too long" — raise
	// it to 4MB up front. One scanner is mandatory: two scanners on the same
	// pipe would drop bytes the first buffered past its cursor.
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	// Disable PI's auto-compaction and auto-retry before the task starts.
	// Both run on their own timers/backoff silently, emit no RPC event we
	// consume, and can stall for tens of minutes while burning the wall cap
	// — the observed 26-min silence after round-1 verifier feedback. The
	// harness's verifier feedback loop is the only retry mechanism the agent
	// needs, and per-run sessions stay small enough that compaction is never
	// triggered when these are off.
	//
	// PI processes stdin lines concurrently (handleInputLine is fire-and-forget),
	// so we write both config commands then wait for their acks before sending
	// the prompt — otherwise the prompt could race ahead of the config.
	cfg := []struct{ id, typ string }{
		{"cfg-compaction", "set_auto_compaction"},
		{"cfg-retry", "set_auto_retry"},
	}
	for _, c := range cfg {
		if err := enc.Encode(map[string]any{"id": c.id, "type": c.typ, "enabled": false}); err != nil {
			_ = cmd.Process.Kill()
			return runtime.RunResult{}, fmt.Errorf("write %s: %w (%s)", c.typ, err, errBuf.String())
		}
	}
	if err := r.awaitConfigAcks(cfg, sc); err != nil {
		_ = cmd.Process.Kill()
		return runtime.RunResult{}, err
	}

	// The initial prompt carries the structured task plus the verdict contract.
	if err := enc.Encode(map[string]any{
		"id":      "1",
		"type":    "prompt",
		"message": initialPrompt,
	}); err != nil {
		_ = cmd.Process.Kill()
		return runtime.RunResult{}, fmt.Errorf("write prompt: %w (%s)", err, errBuf.String())
	}

	// Settle -> verdict -> optional single nudge. The loop stays open across a
	// settle so the agent can be steered once if it omitted the verdict.
	settleErr := r.awaitSettled(in.RunID, sc)
	var v *runtime.Verdict
	if settleErr == nil && !reviewMode {
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

	// External verification feedback loop. A blocked verdict stands down (the
	// runpipe routes that to terminal) and skips verification entirely. Any
	// other verdict — including nil — goes through the loop, which feeds
	// verifier findings back as prompts until it passes, stalls, or exhausts
	// MaxRounds. The agent cannot grade its own homework.
	var vr *verifier.Result
	if verifyMode && (v == nil || v.Status != runtime.VerdictBlocked) {
		verify := r.Verify
		if verify == nil {
			verify = func(ctx context.Context, wt string, fixes []runtime.ClaimedFix) (verifier.Result, error) {
				return (verifier.Verifier{}).Verify(ctx, wt, fixes), nil
			}
		}
		maxRounds := r.MaxRounds
		if maxRounds <= 0 {
			maxRounds = 3
		}

		fixes := []runtime.ClaimedFix(nil)
		if v != nil {
			fixes = v.Fixes
		}
		prevSig := ""
		var prevFailures []verifier.Failure
		for round := 1; round <= maxRounds; round++ {
			res, err := verify(ctx, worktree, fixes)
			if err != nil {
				// A verifier error is red — we never silently skip the gate.
				res = verifier.Result{Failures: []verifier.Failure{
					{Rule: verifier.RuleScannerError, Detail: "verify: " + err.Error()},
				}}
			}
			cur := res
			vr = &cur
			if cur.Pass {
				break
			}
			sig := verifier.Signature(cur.Failures)
			if round == maxRounds || sig == prevSig {
				break // stall or exhausted — ship the known failures in the PR body
			}
			prevSig = sig
			msg := verifier.FormatFeedback(prevFailures, cur.Failures, round, maxRounds)
			if err := enc.Encode(map[string]any{
				"id":      fmt.Sprintf("fb%d", round),
				"type":    "prompt",
				"message": msg,
			}); err != nil {
				r.runLogger(in.RunID).Error("feedback encode failed", "err", err)
				break
			}
			_ = r.awaitSettled(in.RunID, sc)
			if nv := readVerdict(worktree); nv != nil {
				v = nv
				fixes = v.Fixes
			}
			prevFailures = cur.Failures
		}
	}

	var reviewEnv intent.Envelope
	var reviewErr error
	if reviewMode && settleErr == nil {
		reviewEnv, reviewErr = readReviewIntent(worktree, in.RunID)
	}

	// The .bothos directory is harness bookkeeping; it must never reach the
	// diff gate or the PR.
	_ = os.RemoveAll(filepath.Join(worktree, ".bothos"))

	// Close stdin → PI's RPC loop shuts down gracefully, persisting the session.
	_ = stdin.Close()

	if err := cmd.Wait(); err != nil {
		// Distinguish our cancellation from an external kill (research: Go's
		// "signal: killed" is ambiguous; log the context cause + elapsed time).
		r.runLogger(in.RunID).Error("pi rpc exited",
			"err", err, "ctx_err", ctx.Err(), "elapsed", time.Since(start).Round(time.Millisecond), "stderr", errBuf.String())
		return runtime.RunResult{}, fmt.Errorf("pi rpc: %w (ctx=%v)", err, ctx.Err())
	}
	if settleErr != nil {
		// Prompt rejection, or the process exited before settling cleanly.
		return runtime.RunResult{}, settleErr
	}
	if reviewMode {
		if reviewErr != nil {
			return runtime.RunResult{}, reviewErr
		}
		return runtime.RunResult{Intents: []intent.Envelope{reviewEnv}}, nil
	}

	// A blocked verdict is a stand-down: the agent could not complete the change
	// and opens no PR. runpipe routes this to a terminal failure record (never
	// a River retry). Verification was already skipped above.
	if v != nil && v.Status == runtime.VerdictBlocked {
		return runtime.RunResult{Verdict: v}, nil
	}

	// Diff gate: the agent must have committed on its branch (rev-list ahead of
	// origin/HEAD). Dirty-tree (uncommitted) checks belong to the verifier, not
	// here.
	if !hasCommits(worktree) {
		if issueMode && v == nil {
			return runtime.RunResult{Verdict: &runtime.Verdict{
				Status:  runtime.VerdictBlocked,
				Summary: "The agent did not produce a committed change or a usable blocked report. Please provide one concrete implementation decision, then trigger a new labeled run.",
			}}, nil
		}
		return runtime.RunResult{}, fmt.Errorf("agent produced no commits on its branch")
	}

	return runtime.RunResult{
		Intents: []intent.Envelope{openPRIntent(in.RunID, v, vr)},
		Verdict: v,
	}, nil
}

// reportingInstructions is appended to the initial prompt so the agent knows
// how to report its outcome. The verdict is prose for the PR body — never used
// for targeting or gating. It belongs to this harness, not the runtime seam.
const reportingInstructions = `

When you finish, you MUST report your outcome by writing the file
.bothos/verdict.json in the repository root with this exact shape:

{"status": "done" | "done_unverified" | "blocked", "summary": "...", "verification": "...", "fixes": [{"package": "...", "advisory_id": "...", "to": "..."}]}

- "done": the change is complete and you verified it yourself (you ran the
  build/tests/checks you judged appropriate, and they passed). In
  "verification", state exactly what you ran and the result.
- "done_unverified": the change is complete but you could not verify it (for
  example: tests fail for pre-existing or environmental reasons unrelated to
  your change, or no usable test setup exists). In "verification", state what
  you tried and why it is inconclusive.
- "blocked": you cannot complete the change. In "summary", explain why.
- "fixes": (optional) one entry per dependency you upgraded, with the
  package name, the advisory id it resolves (if known), and the target
  version. An external verifier re-scans to confirm these; leave
  "advisory_id" empty to claim any advisory for the package is fixed.
  Omit "fixes" entirely when you did not change any dependency.

How you validate is your decision: the full test suite, targeted tests, a
build, or nothing — whatever fits this change. If a check fails, determine
whether YOUR change caused it: fix what you broke, and do not chase
pre-existing or environmental failures. The .bothos directory is harness
bookkeeping; it is deleted when your run ends and must not be part of the
change itself.`

// reviewReportingInstructions is appended to the review prompt so the agent
// knows how to report its review. The review note is content for a PR comment
// — never used for targeting (the PR number/SHAs come from the grant), and
// never a GitHub approve verdict. Verified/evidence are machine-checked tags
// the harness carries; the agent may only claim [verified] for items it can
// point to exact command output for, everything else stays [opinion].
const reviewReportingInstructions = `

IMPORTANT — you MUST report your review by writing the file
.bothos/review.json in the repository root with this exact shape:

{
  "verdict": "comment" | "request_changes",
  "summary": "one or two sentence summary of the review",
  "comments": [
    {
      "path": "path/to/file",
      "line": 12,
      "side": "RIGHT",
      "body": "the comment text",
      "verified": true,
      "evidence": "exact command you ran and its relevant output"
    }
  ]
}

Rules:
- Use "request_changes" only when you found a blocking defect; otherwise use
  "comment". NEVER use any approve verdict — this bot never approves.
- Every comment must carry an explicit tag in its body, either "[verified]"
  (you ran a deterministic command whose output you quote in the evidence
  field) or "[opinion]" (model judgment, phrased as a question, evidence
  empty). A comment with no tag is invalid; prefer omitting low-confidence
  items.
- This is read-only: never create branches, commit, or push, and never write
  any file other than .bothos/review.json (harness bookkeeping, deleted after
  your run).
- Valid JSON only, nothing else in the file. Omit the evidence field for opinions.`

// awaitConfigAcks reads RPC stdout until the response (ack) for each config
// command has arrived and succeeded, or the process exits. Because PI
// dispatches stdin lines asynchronously, the agent prompt must not be sent
// until these acks confirm the commands were applied; otherwise the agent
// could start running before auto-compaction/auto-retry are disabled.
// Uses the single shared scanner (created before the prompt) so no pipe
// bytes are dropped between the ack phase and the settle phase.
//
// A silent or misbehaving PI must not hang the run: the ack phase is bounded
// by a short timeout (startup config, not agent work — the wall cap applies
// to the agent run itself).
func (r *RPC) awaitConfigAcks(cfg []struct{ id, typ string }, sc *bufio.Scanner) error {
	const ackTimeout = 15 * time.Second

	// The scanner blocks on the pipe; drive it from a goroutine so the
	// timeout below can fire and the Run context can cancel it.
	type ackResult struct {
		err error
	}
	done := make(chan ackResult, 1)
	go func() {
		done <- ackResult{err: r.awaitConfigAcksLoop(cfg, sc)}
	}()

	select {
	case res := <-done:
		return res.err
	case <-time.After(ackTimeout):
		return fmt.Errorf("pi rpc: timed out waiting for config acks (%d pending)", len(cfg))
	}
}

func (r *RPC) awaitConfigAcksLoop(cfg []struct{ id, typ string }, sc *bufio.Scanner) error {
	pending := make(map[string]bool, len(cfg))
	for _, c := range cfg {
		pending[c.id] = true
	}

	var lastErr error
	for len(pending) > 0 && sc.Scan() {
		var ev rpcEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Type != "response" || ev.ID == "" {
			continue
		}
		if !pending[ev.ID] {
			continue
		}
		if ev.Success == nil || !*ev.Success {
			lastErr = fmt.Errorf("pi rpc: %s rejected", ev.Command)
			delete(pending, ev.ID)
			continue
		}
		delete(pending, ev.ID)
	}
	if err := sc.Err(); err != nil && len(pending) > 0 {
		return fmt.Errorf("pi rpc: config ack scan: %w", err)
	}
	if lastErr != nil {
		return lastErr
	}
	if len(pending) > 0 {
		return fmt.Errorf("pi rpc: process exited before config acks (%d pending)", len(pending))
	}
	return nil
}

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
			r.runLogger(runID).Info("rpc tool execution", "tool", ev.ToolName)
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

type reviewOutput struct {
	Verdict  intent.Verdict `json:"verdict"`
	Summary  string         `json:"summary"`
	Comments []struct {
		Path     string `json:"path"`
		Line     int    `json:"line"`
		Side     string `json:"side"`
		Body     string `json:"body"`
		Verified bool   `json:"verified"`
		Evidence string `json:"evidence,omitempty"`
	} `json:"comments"`
}

func readReviewIntent(worktree, runID string) (intent.Envelope, error) {
	path := filepath.Join(worktree, ".bothos", "review.json")
	file, err := os.Open(path)
	if err != nil {
		return intent.Envelope{}, fmt.Errorf("review output: %w", err)
	}
	defer file.Close()

	var output reviewOutput
	dec := json.NewDecoder(file)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&output); err != nil {
		return intent.Envelope{}, fmt.Errorf("review output: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return intent.Envelope{}, fmt.Errorf("review output: multiple JSON values")
		}
		return intent.Envelope{}, fmt.Errorf("review output: %w", err)
	}
	if output.Verdict != intent.VerdictComment && output.Verdict != intent.VerdictRequestChanges {
		return intent.Envelope{}, fmt.Errorf("review output: unsupported verdict %q", output.Verdict)
	}

	review := intent.PostReview{Verdict: output.Verdict, Summary: output.Summary, Comments: make([]intent.ReviewComment, len(output.Comments))}
	for i, comment := range output.Comments {
		review.Comments[i] = intent.ReviewComment{
			Path:     comment.Path,
			Line:     comment.Line,
			Side:     comment.Side,
			Body:     comment.Body,
			Verified: comment.Verified,
			Evidence: comment.Evidence,
		}
	}
	payload, err := json.Marshal(review)
	if err != nil {
		return intent.Envelope{}, fmt.Errorf("review output: %w", err)
	}
	return intent.Envelope{
		SchemaVersion: intent.SchemaVersion,
		RunID:         runID,
		Kind:          intent.KindPostReview,
		Payload:       payload,
	}, nil
}

// hasCommits reports whether the worktree's branch has commits ahead of
// origin/HEAD. The agent must commit its work; the harness never commits for
// it. Dirty-tree (uncommitted) checks belong to the verifier, not here.
func hasCommits(worktree string) bool {
	out, err := exec.Command("git", "-C", worktree, "rev-list", "--count", "origin/HEAD..HEAD").Output()
	if err != nil {
		return false // not a repo / git error ⇒ treat as no commits
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return false
	}
	return n > 0
}

// openPRIntent builds the single deterministic open_pr intent the runpipe and
// executor already expect. Targeting (branch, base, repo) is resolved later by
// the executor from git state and the Grant — the agent contributes content
// only. The title comes from the first line of the verdict summary (or a
// fallback); the body carries the verdict prose plus, when the external
// verifier left known failures, a red-but-committed note — never a silent PR.
func openPRIntent(runID string, v *runtime.Verdict, vr *verifier.Result) intent.Envelope {
	title := "chore(deps): security upgrades"
	if v != nil {
		if line := strings.TrimSpace(strings.Split(v.Summary, "\n")[0]); line != "" {
			title = strings.Join(strings.Fields(line), " ")
			if len(title) > 120 {
				title = title[:120]
			}
		}
	}

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
	body := fmt.Sprintf("%s %s\n\n**Verification:** %s", heading, summary, verification)

	if vr != nil && !vr.Pass {
		var b strings.Builder
		b.WriteString("\n\n**Known failures (external verifier):**\n")
		for _, f := range vr.Failures {
			b.WriteString(fmt.Sprintf("- %s: %s\n", f.Rule, f.Detail))
		}
		body += b.String()
	}

	pc := intent.OpenPR{
		Title: title,
		Body:  body,
		Draft: true,
	}
	raw, _ := json.Marshal(pc)
	return intent.Envelope{SchemaVersion: intent.SchemaVersion, RunID: runID, Kind: intent.KindOpenPR, Payload: raw}
}

// withoutSecrets drops every env var whose key starts with an executor
// write-token prefix. The pi subprocess must never inherit a write
// credential.
func withoutSecrets(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GITHUB_WRITE_TOKEN") || strings.HasPrefix(k, "GITHUB_COMMENT_TOKEN") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

var _ runtime.AgentRuntime = (*RPC)(nil)
