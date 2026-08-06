package agent

import (
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
)

// dirSandbox is a minimal runtime.Sandbox backed by a real directory (the
// "worktree"). RPC runs the (fake) pi with cwd = Worktree().
type dirSandbox struct{ dir string }

func (s dirSandbox) Worktree() string { return s.dir }
func (s dirSandbox) Exec(ctx context.Context, c string, a ...string) (runtime.Output, error) {
	return runtime.Output{}, nil
}

// newRepo creates a real temp git repo with a committed baseline.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"dependencies":{"tar":"7.5.16"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "base")
	return dir
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

func task() runtime.UpgradeTask {
	return runtime.UpgradeTask{
		Package:        "tar",
		CurrentVersion: "7.5.16",
		TargetVersion:  "7.5.19",
		TestCommand:    "npm test",
	}
}

func TestRPCBuildsOpenPRIntentAndSendsPrompt(t *testing.T) {
	repo := newRepo(t)
	r, logf := newRPC(t, map[string]string{"FAKE_PI_EDIT": "1"})

	res, err := r.Run(context.Background(), runtime.RunInput{
		RunID: "run-1", Task: task(), Sandbox: dirSandbox{dir: repo},
		Limits: runtime.Limits{MaxSeconds: 5 * time.Minute},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Intents) != 1 {
		t.Fatalf("want 1 intent, got %d", len(res.Intents))
	}
	env := res.Intents[0]
	if env.Kind != intent.KindOpenPR || env.SchemaVersion != intent.SchemaVersion {
		t.Fatalf("unexpected envelope: kind=%q schema=%d", env.Kind, env.SchemaVersion)
	}
	var pc intent.OpenPR
	if err := json.Unmarshal(env.Payload, &pc); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if pc.Title != "chore(deps): upgrade tar to 7.5.19 (security)" {
		t.Fatalf("title = %q", pc.Title)
	}
	if pc.Topic != "upgrade-tar-7.5.19" {
		t.Fatalf("topic = %q", pc.Topic)
	}
	if !pc.Draft {
		t.Fatalf("draft should be true")
	}
	if pc.Worktree != repo {
		t.Fatalf("worktree = %q, want %q", pc.Worktree, repo)
	}
	// Fake pi ran with cwd=worktree (the edit landed in the repo) and the diff
	// gate saw it.
	if _, err := os.Stat(filepath.Join(repo, "edited.txt")); err != nil {
		t.Fatalf("fake pi did not run in worktree (edited.txt missing): %v", err)
	}
	// The prompt sent to pi contained the structured task.
	if s := readAll(t, logf); !strings.Contains(s, "Package: tar") || !strings.Contains(s, "Target version: 7.5.19") {
		t.Fatalf("prompt not sent or malformed: %q", s)
	}
}

func TestRPCNoChangesIsError(t *testing.T) {
	repo := newRepo(t)
	r, _ := newRPC(t, nil) // no edit => worktree unchanged

	res, err := r.Run(context.Background(), runtime.RunInput{
		RunID: "run-2", Task: task(), Sandbox: dirSandbox{dir: repo},
		Limits: runtime.Limits{MaxSeconds: time.Minute},
	})
	if err == nil || !strings.Contains(err.Error(), "no changes") {
		t.Fatalf("want 'no changes' error, got err=%v", err)
	}
	if len(res.Intents) != 0 {
		t.Fatalf("want 0 intents, got %d", len(res.Intents))
	}
}

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
		RunID: "run-3", Task: task(), Sandbox: dirSandbox{dir: repo},
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
		RunID: "run-4", Task: task(), Sandbox: dirSandbox{dir: repo},
		Limits: runtime.Limits{MaxSeconds: time.Minute},
	})
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("want 'rejected' error, got %v", err)
	}
}
