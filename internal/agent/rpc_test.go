package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/runtime"
	"github.com/shambu2k/bothos/internal/verifier"
)

// dirSandbox is a minimal runtime.Sandbox backed by a real directory (the
// "worktree"). RPC runs the (fake) pi with cwd = Worktree().
type dirSandbox struct{ dir string }

func (s dirSandbox) Worktree() string { return s.dir }
func (s dirSandbox) Exec(ctx context.Context, c string, a ...string) (runtime.Output, error) {
	return runtime.Output{}, nil
}

// newRepo creates a real temp git repo cloned from a bare seed (origin/HEAD =
// main), so the commits-ahead gate can be satisfied by an agent commit.
func newRepo(t *testing.T) string {
	t.Helper()
	gitDir := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git")
		cmd.Args = append(cmd.Args, "-C", dir)
		cmd.Args = append(cmd.Args, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v (%s)", args, dir, err, out)
		}
	}
	seed := t.TempDir()
	gitDir(seed, "init", "-q", "-b", "main")
	gitDir(seed, "config", "user.email", "t@example.com")
	gitDir(seed, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(seed, "package.json"), []byte(`{"dependencies":{"tar":"7.5.16"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDir(seed, "add", ".")
	gitDir(seed, "commit", "-qm", "base")

	origin := t.TempDir()
	gitDir(origin, "init", "-q", "--bare", "-b", "main")
	gitDir(seed, "remote", "add", "origin", origin)
	gitDir(seed, "push", "-qu", "origin", "main")

	wt := filepath.Join(t.TempDir(), "repo")
	cmd := exec.Command("git", "clone", "-q", origin, wt)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v (%s)", err, out)
	}
	return wt
}

// newRPC builds an RPC whose piBin is the fake script, with the given fake
// env behaviors, scoped to a fresh temp session dir + prompt log.
func newRPC(t *testing.T, fakeEnv map[string]string) (*RPC, string) {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "fake_pi.sh"))
	if err != nil {
		t.Fatal(err)
	}
	logf := filepath.Join(t.TempDir(), "in.jsonl")
	os.Setenv("FAKE_PI_PROMPT_FILE", logf)
	for k, v := range fakeEnv {
		os.Setenv(k, v)
	}
	t.Cleanup(func() {
		os.Unsetenv("FAKE_PI_PROMPT_FILE")
		for k := range fakeEnv {
			os.Unsetenv(k)
		}
	})
	r := NewPIRPC(abs, "openrouter/deepseek/deepseek-v4-flash-0731", t.TempDir(), true)
	r.WaitDelay = 800 * time.Millisecond
	return r, logf
}

func readAll(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func task() runtime.SecurityTask {
	return runtime.SecurityTask{BaseRef: "main"}
}

// passVerify returns a verifier func that reports Green returns from i.
func scriptedVerify(results ...verifier.Result) func(ctx context.Context, wt string, fixes []runtime.ClaimedFix) (verifier.Result, error) {
	i := 0
	return func(ctx context.Context, wt string, fixes []runtime.ClaimedFix) (verifier.Result, error) {
		if i < len(results) {
			r := results[i]
			i++
			return r, nil
		}
		last := results[len(results)-1]
		return last, nil
	}
}

func redResult(detail string) verifier.Result {
	return verifier.Result{Failures: []verifier.Failure{{Rule: verifier.RuleVulnPresent, Detail: detail}}}
}

func openPR(t *testing.T, res runtime.RunResult) intent.OpenPR {
	t.Helper()
	for _, env := range res.Intents {
		if env.Kind == intent.KindOpenPR {
			var pc intent.OpenPR
			if err := json.Unmarshal(env.Payload, &pc); err != nil {
				t.Fatalf("payload: %v", err)
			}
			return pc
		}
	}
	t.Fatal("no open_pr intent")
	return intent.OpenPR{}
}

func promptCount(t *testing.T, logf string) int {
	t.Helper()
	n := 0
	for _, l := range strings.Split(readAll(t, logf), "\n") {
		if strings.Contains(l, `"type":"prompt"`) {
			n++
		}
	}
	return n
}

// ---------- green first round ----------

func TestRPCGreenFirstRoundOnePrompt(t *testing.T) {
	repo := newRepo(t)
	r, logf := newRPC(t, map[string]string{
		"FAKE_PI_EDIT":    "1",
		"FAKE_PI_VERDICT": `{"status":"done","summary":"bumped tar","verification":"ran checks","fixes":[{"package":"tar","advisory_id":"GHSA-abc","to":"7.5.19"}]}`,
	})
	r.Verify = scriptedVerify(verifier.Result{Pass: true})

	res, err := r.Run(context.Background(), runtime.RunInput{
		RunID: "run1", Task: task(), Sandbox: dirSandbox{dir: repo},
		Limits: runtime.Limits{MaxSeconds: 5 * time.Minute},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Intents) != 1 || res.Intents[0].Kind != intent.KindOpenPR {
		t.Fatalf("want 1 open_pr intent, got %d", len(res.Intents))
	}
	if res.Verdict == nil || res.Verdict.Status != runtime.VerdictDone {
		t.Fatalf("verdict = %+v", res.Verdict)
	}
	// Title comes from the first line of the verdict summary.
	if pc := openPR(t, res); !pc.Draft || pc.Title != "bumped tar" {
		t.Fatalf("title/draft = %q/%v", pc.Title, pc.Draft)
	}
	// .bothos removed.
	if _, err := os.Stat(filepath.Join(repo, ".bothos")); !os.IsNotExist(err) {
		t.Fatalf(".bothos should be removed, stat err=%v", err)
	}
	// Exactly one prompt: no nudge, no feedback.
	if got := promptCount(t, logf); got != 1 {
		t.Fatalf("want 1 prompt, got %d", got)
	}
	// Prompt content includes the runID-interpolated branch contract + scanner.
	s := readAll(t, logf)
	if !strings.Contains(s, "bot/run1-") || !strings.Contains(s, "osv-scanner") {
		t.Fatalf("prompt missing runID/osv-scanner markers:\n%s", s)
	}
}

// ---------- red -> green (feedback) ----------

func TestRPCFeedbackRedToGreen(t *testing.T) {
	repo := newRepo(t)
	r, logf := newRPC(t, map[string]string{
		"FAKE_PI_EDIT":    "1",
		"FAKE_PI_VERDICT": `{"status":"done","summary":"bumped tar","verification":"ran checks","fixes":[{"package":"tar","advisory_id":"GHSA-abc","to":"7.5.19"}]}`,
	})
	// First verify red (vuln still present), second verify green.
	r.Verify = scriptedVerify(
		redResult("tar GHSA-abc still present at 7.5.16"),
		verifier.Result{Pass: true},
	)

	res, err := r.Run(context.Background(), runtime.RunInput{
		RunID: "run2", Task: task(), Sandbox: dirSandbox{dir: repo},
		Limits: runtime.Limits{MaxSeconds: 5 * time.Minute},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Initial + one feedback prompt = 2, and the feedback carried the detail.
	if got := promptCount(t, logf); got != 2 {
		t.Fatalf("want 2 prompts, got %d", got)
	}
	if s := readAll(t, logf); !strings.Contains(s, "tar GHSA-abc still present") {
		t.Fatalf("feedback missing failure detail:\n%s", s)
	}
	// Second-round green: no Known-failures section in the PR body.
	if body := openPR(t, res).Body; strings.Contains(body, "Known failures") {
		t.Fatalf("green run must not carry Known failures:\n%s", body)
	}
}

// ---------- stall detection ----------

func TestRPCFeedbackStallStopsEarly(t *testing.T) {
	repo := newRepo(t)
	r, logf := newRPC(t, map[string]string{
		"FAKE_PI_EDIT":    "1",
		"FAKE_PI_VERDICT": `{"status":"done","summary":"bumped tar","verification":"ran checks","fixes":[{"package":"tar"}]}`,
	})
	// Same failure set every round -> stall detected at round 2 (MaxRounds 3).
	r.Verify = scriptedVerify(redResult("tar GHSA-abc still present at 7.5.16"))
	r.MaxRounds = 3

	res, err := r.Run(context.Background(), runtime.RunInput{
		RunID: "run3", Task: task(), Sandbox: dirSandbox{dir: repo},
		Limits: runtime.Limits{MaxSeconds: 5 * time.Minute},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// initial + 1 feedback = 2 prompts (stall stops before the 3rd round prompt).
	if got := promptCount(t, logf); got != 2 {
		t.Fatalf("stall: want 2 prompts, got %d", got)
	}
	// Second verify had the same signature, so we stopped red with known
	// failures in the PR body.
	if body := openPR(t, res).Body; !strings.Contains(body, "Known failures (external verifier)") {
		t.Fatalf("stalled red run must list known failures:\n%s", body)
	}
}

// ---------- always red -> exhausted -> Known failures in PR body ----------

func TestRPCAlwaysRedExhaustsRounds(t *testing.T) {
	repo := newRepo(t)
	r, logf := newRPC(t, map[string]string{
		"FAKE_PI_EDIT":    "1",
		"FAKE_PI_VERDICT": `{"status":"done","summary":"bumped tar","verification":"ran checks","fixes":[{"package":"tar"}]}`,
	})
	r.Verify = scriptedVerify(redResult("tar GHSA-x still present at 7.5.16"))
	r.MaxRounds = 2

	res, err := r.Run(context.Background(), runtime.RunInput{
		RunID: "run4", Task: task(), Sandbox: dirSandbox{dir: repo},
		Limits: runtime.Limits{MaxSeconds: 5 * time.Minute},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := promptCount(t, logf); got != r.MaxRounds {
		t.Fatalf("always-red: want %d prompts, got %d", r.MaxRounds, got)
	}
	if body := openPR(t, res).Body; !strings.Contains(body, "Known failures (external verifier)") {
		t.Fatalf("exhausted red run must list known failures:\n%s", body)
	}
}

// ---------- blocked stands down, Verify never called ----------

func TestRPCBlockedSkipsVerifyAndYieldsNoIntent(t *testing.T) {
	repo := newRepo(t)
	verifyCalls := 0
	r, _ := newRPC(t, map[string]string{
		"FAKE_PI_EDIT":    "1",
		"FAKE_PI_VERDICT": `{"status":"blocked","summary":"cannot migrate: API removed","verification":""}`,
	})
	r.Verify = func(ctx context.Context, wt string, fixes []runtime.ClaimedFix) (verifier.Result, error) {
		verifyCalls++
		return verifier.Result{Pass: true}, nil
	}

	res, err := r.Run(context.Background(), runtime.RunInput{
		RunID: "run5", Task: task(), Sandbox: dirSandbox{dir: repo},
		Limits: runtime.Limits{MaxSeconds: 5 * time.Minute},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if verifyCalls != 0 {
		t.Fatalf("Verify called %d times on a blocked run", verifyCalls)
	}
	if len(res.Intents) != 0 {
		t.Fatalf("blocked run must emit no open_pr intent, got %d", len(res.Intents))
	}
	if res.Verdict == nil || res.Verdict.Status != runtime.VerdictBlocked {
		t.Fatalf("verdict = %+v", res.Verdict)
	}
}

// ---------- no commits -> gate error ----------

func TestRPCNoCommitsIsError(t *testing.T) {
	repo := newRepo(t)
	r, _ := newRPC(t, map[string]string{ // no FAKE_PI_EDIT => nothing committed
		"FAKE_PI_VERDICT": `{"status":"done","summary":"bumped","verification":"ran checks"}`,
	})
	r.Verify = scriptedVerify(verifier.Result{Pass: true})

	_, err := r.Run(context.Background(), runtime.RunInput{
		RunID: "run6", Task: task(), Sandbox: dirSandbox{dir: repo},
		Limits: runtime.Limits{MaxSeconds: time.Minute},
	})
	if err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Fatalf("want 'no commits' error, got err=%v", err)
	}
}

// ---------- nudge for a missing verdict still verifies ----------

func TestRPCNudgesForMissingVerdictThenVerifies(t *testing.T) {
	repo := newRepo(t)
	r, logf := newRPC(t, map[string]string{
		"FAKE_PI_EDIT":              "1",
		"FAKE_PI_VERDICT":           `{"status":"done","summary":"bumped","verification":"ran checks"}`,
		"FAKE_PI_VERDICT_ON_PROMPT": "2",
	})
	r.Verify = scriptedVerify(verifier.Result{Pass: true})

	res, err := r.Run(context.Background(), runtime.RunInput{
		RunID: "run7", Task: task(), Sandbox: dirSandbox{dir: repo},
		Limits: runtime.Limits{MaxSeconds: time.Minute},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := promptCount(t, logf); got != 2 {
		t.Fatalf("want 2 prompts (initial + nudge), got %d", got)
	}
	if res.Verdict == nil || res.Verdict.Status != runtime.VerdictDone {
		t.Fatalf("verdict = %+v", res.Verdict)
	}
}

func TestRPCInvalidVerdictTreatedAsAbsent(t *testing.T) {
	repo := newRepo(t)
	r, logf := newRPC(t, map[string]string{
		"FAKE_PI_EDIT":    "1",
		"FAKE_PI_VERDICT": `{"status":"great"}`,
	})
	r.Verify = scriptedVerify(verifier.Result{Pass: true})

	res, err := r.Run(context.Background(), runtime.RunInput{
		RunID: "run8", Task: task(), Sandbox: dirSandbox{dir: repo},
		Limits: runtime.Limits{MaxSeconds: time.Minute},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Verdict != nil {
		t.Fatalf("invalid verdict should be nil, got %+v", res.Verdict)
	}
	if got := promptCount(t, logf); got != 2 {
		t.Fatalf("invalid status should take the nudge path (2 prompts), got %d", got)
	}
	if body := openPR(t, res).Body; !strings.Contains(body, "the agent did not report a run status") {
		t.Fatalf("body missing nil-verdict note:\n%s", body)
	}
}

// ---------- agent_end fallback settles ----------

func TestRPCRetriesSettleViaAgentEndFallback(t *testing.T) {
	repo := newRepo(t)
	r, _ := newRPC(t, map[string]string{
		"FAKE_PI_EDIT":      "1",
		"FAKE_PI_AGENT_END": "1",
		"FAKE_PI_VERDICT":   `{"status":"done","summary":"bumped","verification":"ran checks"}`,
	})
	r.Verify = scriptedVerify(verifier.Result{Pass: true})

	res, err := r.Run(context.Background(), runtime.RunInput{
		RunID: "runfb", Task: task(), Sandbox: dirSandbox{dir: repo},
		Limits: runtime.Limits{MaxSeconds: time.Minute},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Verdict == nil || res.Verdict.Status != runtime.VerdictDone {
		t.Fatalf("verdict = %+v", res.Verdict)
	}
}

// ---------- lifecycle: cancel / rejected prompt ----------

func TestRPCGracefulCancelSurfacesCause(t *testing.T) {
	repo := newRepo(t)
	r, _ := newRPC(t, map[string]string{
		"FAKE_PI_NO_AGENT_END": "1", // never finishes its turn
		"FAKE_PI_IGNORE_TERM":  "1", // ignores SIGTERM => WaitDelay escalation
	})
	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	_, err := r.Run(ctx, runtime.RunInput{
		RunID: "run3", Task: task(), Sandbox: dirSandbox{dir: repo},
		Limits: runtime.Limits{MaxSeconds: 10 * time.Minute},
	})
	if err == nil {
		t.Fatal("want error after cancel")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("error should surface ctx cause: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancel took too long: %v", elapsed)
	}
}

func TestRPCRejectedPromptIsError(t *testing.T) {
	repo := newRepo(t)
	r, _ := newRPC(t, map[string]string{"FAKE_PI_REJECT": "1"})

	_, err := r.Run(context.Background(), runtime.RunInput{
		RunID: "run4", Task: task(), Sandbox: dirSandbox{dir: repo},
		Limits: runtime.Limits{MaxSeconds: time.Minute},
	})
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("want 'rejected' error, got %v", err)
	}
}

func TestRPCIssueWithoutVerdictOrCommitStandsDown(t *testing.T) {
	repo := newRepo(t)
	r, _ := newRPC(t, nil)

	result, err := r.Run(context.Background(), runtime.RunInput{
		RunID:   "issue-no-output",
		Task:    runtime.IssueTask{IssueNumber: 42, RepoURL: "https://github.com/acme/widget.git", BaseRef: "main"},
		Sandbox: dirSandbox{dir: repo}, Limits: runtime.Limits{MaxSeconds: time.Minute},
	})
	if err != nil {
		t.Fatalf("issue stand-down: %v", err)
	}
	if result.Verdict == nil || result.Verdict.Status != runtime.VerdictBlocked || len(result.Intents) != 0 {
		t.Fatalf("result = %+v, want blocked verdict without intents", result)
	}
	if !strings.Contains(result.Verdict.Summary, "did not produce a committed change") {
		t.Fatalf("fallback summary = %q", result.Verdict.Summary)
	}
}

func TestRPCIssueProducesDraftIntentWithoutDependencyVerifier(t *testing.T) {
	repo := newRepo(t)
	r, logf := newRPC(t, map[string]string{
		"FAKE_PI_EDIT":    "1",
		"FAKE_PI_BRANCH":  "bot/issue-run-fix-login",
		"FAKE_PI_VERDICT": `{"status":"done","summary":"fix login validation","verification":"go test ./..."}`,
	})
	verifyCalls := 0
	r.Verify = func(context.Context, string, []runtime.ClaimedFix) (verifier.Result, error) {
		verifyCalls++
		return verifier.Result{Pass: true}, nil
	}

	result, err := r.Run(context.Background(), runtime.RunInput{
		RunID:   "issue-run",
		Task:    runtime.IssueTask{IssueNumber: 42, Title: "Login accepts empty passwords", Body: "Reproduce with an empty password.", RepoURL: "https://github.com/acme/widget.git", BaseRef: "main"},
		Sandbox: dirSandbox{dir: repo}, Limits: runtime.Limits{MaxSeconds: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	if verifyCalls != 0 || len(result.Intents) != 1 || result.Intents[0].Kind != intent.KindOpenPR {
		t.Fatalf("result=%+v verifier calls=%d", result, verifyCalls)
	}
	if prompt := readAll(t, logf); !strings.Contains(prompt, "issue #42") || !strings.Contains(prompt, "Login accepts empty passwords") {
		t.Fatalf("issue prompt missing task context:\n%s", prompt)
	}
}

func TestRPCReviewReturnsOpinionWithoutCommitOrVerifier(t *testing.T) {
	repo := newRepo(t)
	r, logf := newRPC(t, map[string]string{
		"FAKE_PI_REVIEW": `{"verdict":"request_changes","summary":"check this","comments":[{"path":"package.json","line":1,"side":"RIGHT","body":"version looks risky"}]}`,
	})
	verifyCalls := 0
	r.Verify = func(context.Context, string, []runtime.ClaimedFix) (verifier.Result, error) {
		verifyCalls++
		return verifier.Result{Pass: true}, nil
	}

	res, err := r.Run(context.Background(), runtime.RunInput{
		RunID: "review1",
		Task: runtime.ReviewTask{
			PRNumber: 9,
			BaseSHA:  strings.Repeat("1", 40),
			HeadSHA:  strings.Repeat("2", 40),
			RepoURL:  "https://github.com/acme/widget.git",
		},
		Sandbox: dirSandbox{dir: repo},
		Limits:  runtime.Limits{MaxSeconds: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	if verifyCalls != 0 {
		t.Fatalf("review invoked verifier %d times", verifyCalls)
	}
	if res.Verdict != nil || len(res.Intents) != 1 || res.Intents[0].Kind != intent.KindPostReview {
		t.Fatalf("review result = %+v", res)
	}
	var review intent.PostReview
	if err := json.Unmarshal(res.Intents[0].Payload, &review); err != nil {
		t.Fatal(err)
	}
	if len(review.Comments) != 1 || review.Comments[0].Verified || review.Comments[0].Evidence != "" {
		t.Fatalf("model comment was trusted: %+v", review.Comments)
	}
	if got := readAll(t, logf); !strings.Contains(got, "read-only") || !strings.Contains(got, ".bothos/review.json") {
		t.Fatalf("review prompt must be read-only and carry the review.json report contract:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(repo, ".bothos")); !os.IsNotExist(err) {
		t.Fatalf(".bothos should be removed, stat err=%v", err)
	}
}

func TestRPCReviewThreadsVerifiedAndEvidence(t *testing.T) {
	repo := newRepo(t)
	r, _ := newRPC(t, map[string]string{
		"FAKE_PI_REVIEW": `{"verdict":"comment","summary":"mostly fine","comments":[
			{"path":"PHASE3_LIVE_PROBE.md","line":5,"side":"RIGHT","body":"[verified] secret-like pattern","verified":true,"evidence":"grep -nE 'sk-live-probe' PHASE3_LIVE_PROBE.md"},
			{"path":"src/app.go","line":3,"side":"RIGHT","body":"[opinion] could we make this clearer?","verified":false}
		]}`,
	})
	verifyCalls := 0
	r.Verify = func(context.Context, string, []runtime.ClaimedFix) (verifier.Result, error) {
		verifyCalls++
		return verifier.Result{Pass: true}, nil
	}

	res, err := r.Run(context.Background(), runtime.RunInput{
		RunID: "review3",
		Task: runtime.ReviewTask{
			PRNumber: 11, BaseSHA: strings.Repeat("1", 40), HeadSHA: strings.Repeat("2", 40),
			RepoURL: "https://github.com/acme/widget.git",
		},
		Sandbox: dirSandbox{dir: repo},
		Limits:  runtime.Limits{MaxSeconds: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	var review intent.PostReview
	if err := json.Unmarshal(res.Intents[0].Payload, &review); err != nil {
		t.Fatal(err)
	}
	if len(review.Comments) != 2 {
		t.Fatalf("want 2 comments, got %d", len(review.Comments))
	}
	if !review.Comments[0].Verified || review.Comments[0].Evidence == "" {
		t.Fatalf("verified comment lost evidence: %+v", review.Comments[0])
	}
	if review.Comments[1].Verified || review.Comments[1].Evidence != "" {
		t.Fatalf("opinion comment wrongly trusted: %+v", review.Comments[1])
	}
	if verifyCalls != 0 {
		t.Fatalf("review invoked verifier %d times", verifyCalls)
	}
}

func TestRPCReviewRejectsMissingInvalidAndExtendedOutput(t *testing.T) {
	tests := []struct {
		name   string
		review string
	}{
		{name: "missing"},
		{name: "invalid", review: `{`},
		{name: "extra field", review: `{"verdict":"comment","summary":"x","comments":[],"target":"elsewhere"}`},
		{name: "unsupported verdict", review: `{"verdict":"approve","summary":"x","comments":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newRepo(t)
			r, _ := newRPC(t, map[string]string{"FAKE_PI_REVIEW": tt.review})
			_, err := r.Run(context.Background(), runtime.RunInput{
				RunID: "bad-review", Task: runtime.ReviewTask{PRNumber: 9}, Sandbox: dirSandbox{dir: repo},
				Limits: runtime.Limits{MaxSeconds: time.Minute},
			})
			if err == nil {
				t.Fatal("expected review output error")
			}
		})
	}
}

// ---------- env hygiene: write token stripped ----------

func TestWithoutSecretsStripsWriteTokens(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"GITHUB_WRITE_TOKEN=secret-global",
		"GITHUB_WRITE_TOKEN_ACME=secret-account",
		"GITHUB_COMMENT_TOKEN=secret-comment",
		"GITHUB_COMMENT_TOKEN_ACME=secret-comment-account",
		"GITHUB_READ_TOKEN=readonly-ok",
		"FOO=bar",
	}
	out := withoutSecrets(env)
	for _, kv := range out {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GITHUB_WRITE_TOKEN") || strings.HasPrefix(k, "GITHUB_COMMENT_TOKEN") {
			t.Fatalf("write token leaked into agent env: %q", kv)
		}
	}
	for _, want := range []string{"PATH=/usr/bin", "GITHUB_READ_TOKEN=readonly-ok", "FOO=bar"} {
		found := false
		for _, kv := range out {
			if kv == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected %q preserved in env: %v", want, out)
		}
	}
}

// TestRPCTokenTooLong guards the scanner buffer: PI can emit single lines far
// over the bufio 64KB default, which would abort the run with "token too long".
// The scanner must accept lines up to 4MB (regression for the redesign drop).
func TestRPCTokenTooLong(t *testing.T) {
	long := strings.Repeat("x", 300*1024) + "\n" // 300KB single line
	got := ""
	sc := bufio.NewScanner(strings.NewReader(long))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		got = sc.Text()
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner rejected long line: %v", err)
	}
	if len(got) != 300*1024 {
		t.Fatalf("scanned %d bytes, want %d", len(got), 300*1024)
	}
}

// TestAwaitConfigAcks: the two config commands must each be acked as success
// before the prompt is sent. A rejected command is a hard error.
func TestAwaitConfigAcks(t *testing.T) {
	cfg := []struct{ id, typ string }{
		{"cfg-compaction", "set_auto_compaction"},
		{"cfg-retry", "set_auto_retry"},
	}

	// Both acked -> nil.
	stream := "{\"id\":\"cfg-retry\",\"type\":\"response\",\"command\":\"set_auto_retry\",\"success\":true}\n" +
		"{\"type\":\"session\"}\n" +
		"{\"id\":\"cfg-compaction\",\"type\":\"response\",\"command\":\"set_auto_compaction\",\"success\":true}\n"
	sc := bufio.NewScanner(strings.NewReader(stream))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	if err := (&RPC{}).awaitConfigAcks(cfg, sc); err != nil {
		t.Fatalf("happy path: %v", err)
	}

	// One command rejected -> error naming it.
	rejected := "{\"id\":\"cfg-compaction\",\"type\":\"response\",\"command\":\"set_auto_compaction\",\"success\":false,\"error\":\"unknown command\"}\n" +
		"{\"id\":\"cfg-retry\",\"type\":\"response\",\"command\":\"set_auto_retry\",\"success\":true}\n"
	sc2 := bufio.NewScanner(strings.NewReader(rejected))
	sc2.Buffer(make([]byte, 64*1024), 4*1024*1024)
	if err := (&RPC{}).awaitConfigAcks(cfg, sc2); err == nil {
		t.Fatal("expected error for rejected config command")
	} else if !strings.Contains(err.Error(), "set_auto_compaction") {
		t.Fatalf("error should name the rejected command: %v", err)
	}

	// EOF before all acks -> error.
	partial := "{\"id\":\"cfg-retry\",\"type\":\"response\",\"command\":\"set_auto_retry\",\"success\":true}\n"
	sc3 := bufio.NewScanner(strings.NewReader(partial))
	sc3.Buffer(make([]byte, 64*1024), 4*1024*1024)
	if err := (&RPC{}).awaitConfigAcks(cfg, sc3); err == nil {
		t.Fatal("expected error on EOF before all acks")
	}
}

// ---------- session usage reporting ----------

// TestRPCReportsSessionUsage: the harness asks the running pi for cumulative
// session stats (get_session_stats) before shutdown and threads the values
// into the RunResult, so the ledger can record tokens/cost per run.
func TestRPCReportsSessionUsage(t *testing.T) {
	repo := newRepo(t)
	r, _ := newRPC(t, map[string]string{
		"FAKE_PI_EDIT":       "1",
		"FAKE_PI_VERDICT":    `{"status":"done","summary":"bumped tar","verification":"ran checks"}`,
		"FAKE_PI_TOKENS_IN":  "777",
		"FAKE_PI_TOKENS_OUT": "22",
		"FAKE_PI_COST":       "0.0420",
	})
	r.Verify = scriptedVerify(verifier.Result{Pass: true})

	res, err := r.Run(context.Background(), runtime.RunInput{
		RunID: "run-usage", Task: task(), Sandbox: dirSandbox{dir: repo},
		Limits: runtime.Limits{MaxSeconds: 5 * time.Minute},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TokensIn != 777 || res.TokensOut != 22 || res.CostUSD != 0.0420 {
		t.Fatalf("usage = in=%d out=%d cost=%f, want 777/22/0.042000", res.TokensIn, res.TokensOut, res.CostUSD)
	}
	if res.Model == "" {
		t.Fatal("model not reported")
	}
}

// TestSessionUsageDegradesOnSilentPi: a pi build that does not answer
// get_session_stats (or an EOF/timeout) must never fail the run — the stats
// reader yields the zero value and sessionUsage returns zeros.
func TestSessionUsageDegradesOnSilentPi(t *testing.T) {
	// Stream of plain events with no get_session_stats response.
	stream := "{\"type\":\"session\"}\n" +
		"{\"type\":\"agent_settled\"}\n"
	sc := bufio.NewScanner(strings.NewReader(stream))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	if got := (&RPC{}).awaitSessionStats(sc); got.Type != "" || got.Command != "" {
		t.Fatalf("awaitSessionStats on non-stats stream = %+v, want zero value", got)
	}

	// Malformed stats JSON is skipped, EOF is tolerated.
	bad := "{\"type\":\"message_update\",\"usage\":{broken}}\n"
	sc2 := bufio.NewScanner(strings.NewReader(bad))
	sc2.Buffer(make([]byte, 64*1024), 4*1024*1024)
	if got := (&RPC{}).awaitSessionStats(sc2); got.Type != "" {
		t.Fatalf("awaitSessionStats over malformed stream = %+v, want zero value", got)
	}
}
