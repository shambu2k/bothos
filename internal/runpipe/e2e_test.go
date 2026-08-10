// Package runpipe_test holds the end-to-end contract test: fake pi + real
// executor + real git, asserting the exact seam guarantees that eliminated the
// branch-derived-in-two-places / hardcoded-base / worktree-transported /
// per-account-token-env bug class. It is the regression net the redesign is
// for.
package runpipe_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shambu2k/bothos/internal/agent"
	"github.com/shambu2k/bothos/internal/executor"
	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/runpipe"
	"github.com/shambu2k/bothos/internal/runtime"
	"github.com/shambu2k/bothos/internal/upgrade"
	"github.com/shambu2k/bothos/internal/verifier"
)

// ---------- stubs ----------

type stubStore struct {
	run    ledger.Run
	status ledger.RunStatus
}

func (s *stubStore) RunByID(ctx context.Context, id string) (ledger.Run, error) { return s.run, nil }
func (s *stubStore) SetRunStatus(ctx context.Context, id string, st ledger.RunStatus) error {
	s.status = st
	return nil
}
func (s *stubStore) SetRunFailure(ctx context.Context, id, reason string) error { return nil }

type staticStore struct{}

func (staticStore) Resolve(ctx context.Context, accountID string, scope intent.TokenScope) (string, error) {
	return "pat-static-test", nil
}

type memLedger struct{ refs map[string]string }

func (m *memLedger) Lookup(ctx context.Context, key string) (string, bool, error) {
	ref, ok := m.refs[key]
	return ref, ok, nil
}
func (m *memLedger) Record(ctx context.Context, key, runID, ref string) error {
	m.refs[key] = ref
	return nil
}

type recWriter struct {
	openPR *executor.OpenPRWrite
	pushed string
}

func (w *recWriter) OpenPR(ctx context.Context, c executor.Credential, s executor.OpenPRWrite) (string, error) {
	w.openPR = &s
	return "acme/repo#42", nil
}
func (w *recWriter) PushBranch(ctx context.Context, c executor.Credential, branch, worktree string) error {
	w.pushed = branch
	return nil
}
func (w *recWriter) UpdatePR(ctx context.Context, c executor.Credential, s executor.UpdatePRWrite) (string, error) {
	return "", nil
}
func (w *recWriter) PostReview(ctx context.Context, c executor.Credential, s executor.PostReviewWrite) (string, error) {
	return "", nil
}
func (w *recWriter) PostComment(ctx context.Context, c executor.Credential, s executor.PostCommentWrite) (string, error) {
	return "", nil
}
func (w *recWriter) SetLabels(ctx context.Context, c executor.Credential, s executor.SetLabelsWrite) (string, error) {
	return "", nil
}

type dirSandbox struct{ dir string }

func (s dirSandbox) Worktree() string { return s.dir }
func (s dirSandbox) Exec(ctx context.Context, c string, a ...string) (runtime.Output, error) {
	return runtime.Output{}, nil
}

// recordingExecutor captures the envelope+grant the pipeline handed the
// executor, then delegates to the real one (for the dedup re-execution assert).
type recordingExecutor struct {
	inner   runpipe.Executor
	captured *intent.Envelope
}

func (r *recordingExecutor) Execute(ctx context.Context, env intent.Envelope, g intent.Grant, wt string) (executor.Result, error) {
	cp := env
	r.captured = &cp
	return r.inner.Execute(ctx, env, g, wt)
}

// ---------- seed repo ----------

// seedRepo builds a bare origin whose default branch is "main", with one seed
// commit, and returns the origin path. The sandboxer clones from it, so
// origin/HEAD must resolve to main in the clone.
func seedRepo(t *testing.T) string {
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
	return origin
}

// TestE2EOneDraftPRWithGitDerivedHeadAndBase is the step-9 contract test:
// fake pi (commits on bot/<runID>-security-fixes) + real executor + real git
// produce exactly one draft PR whose head is the agent's branch and whose base
// is the seed repo's default branch — with the worktree transported
// out-of-band and branch/base read from git state, never from the envelope.
func TestE2EOneDraftPRWithGitDerivedHeadAndBase(t *testing.T) {
	origin := seedRepo(t)
	runID := "run-e2e-1"

	// fake pi trajectory: branch + commit + verdict with a claimed fix.
	logf := filepath.Join(t.TempDir(), "in.jsonl")
	t.Setenv("FAKE_PI_PROMPT_FILE", logf)
	t.Setenv("FAKE_PI_EDIT", "1")
	t.Setenv("FAKE_PI_BRANCH", "bot/"+runID+"-security-fixes")
	t.Setenv("FAKE_PI_VERDICT", `{"status":"done","summary":"bumped tar to 7.5.19","verification":"npm test ok","fixes":[{"package":"tar","advisory_id":"GHSA-abc","to":"7.5.19"}]}`)
	t.Cleanup(func() {
		os.Unsetenv("FAKE_PI_PROMPT_FILE")
		os.Unsetenv("FAKE_PI_EDIT")
		os.Unsetenv("FAKE_PI_BRANCH")
		os.Unsetenv("FAKE_PI_VERDICT")
	})

	abs, err := filepath.Abs("../agent/testdata/fake_pi.sh")
	if err != nil {
		t.Fatal(err)
	}
	pi := agent.NewPIRPC(abs, "openrouter/deepseek/deepseek-v4-flash-0731", t.TempDir(), true)
	pi.WaitDelay = 800 * time.Millisecond
	pi.Verify = func(ctx context.Context, wt string, fixes []runtime.ClaimedFix) (verifier.Result, error) {
		return verifier.Result{Pass: true}, nil
	}
	pi.MaxRounds = 3

	grant := intent.Grant{
		RunID:        runID,
		Repo:         intent.Repo{Owner: "acme", Name: "repo", AccountID: "acme"},
		Scope:        intent.Scope{Kind: intent.ScopeScheduled, BaseRef: "main"},
		AllowedKinds: []intent.Kind{intent.KindOpenPR},
		TokenScope:   intent.TokenContentsWrite,
		Limits:       intent.DefaultLimits(),
		IssuedAt:     time.Now().Add(-time.Minute),
		ExpiresAt:    time.Now().Add(45 * time.Minute),
		DeniedPaths:  []string{".env", "**/*.key"},
	}
	grantJSON, _ := json.Marshal(grant)
	metaJSON, _ := json.Marshal(runpipe.UpgradeMeta{Scope: "security", BaseRef: "main"})

	st := &stubStore{run: ledger.Run{
		ID: runID, RepoID: "acme/repo", Trigger: "upgrade",
		ScopeKind: "scheduled", Grant: grantJSON, Decision: "allow",
		Status: ledger.RunQueued, Meta: metaJSON,
	}}

	wtLedger := &memLedger{refs: map[string]string{}}
	writer := &recWriter{}
	realExec := executor.NewExecutor(staticStore{}, wtLedger, writer, upgrade.GitDiff{}, time.Now)
	recExec := &recordingExecutor{inner: realExec}

	var worktree string
	p := &runpipe.Pipeline{
		Store: st,
		Agent: pi,
		Exec:  recExec,
		Sandbox: func(ctx context.Context, repo string) (runtime.Sandbox, error) {
			dir := t.TempDir()
			wt := filepath.Join(dir, "repo")
			cmd := exec.Command("git", "clone", "-q", origin, wt)
			if out, err := cmd.CombinedOutput(); err != nil {
				return nil, fmt.Errorf("clone: %w (%s)", err, out)
			}
			worktree = wt
			return dirSandbox{dir: wt}, nil
		},
	}

	ref, err := p.Run(context.Background(), runID)
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if ref == "" {
		t.Fatal("returned ref must be non-empty")
	}
	if st.status != ledger.RunSucceeded {
		t.Fatalf("store status = %q, want succeeded", st.status)
	}

	// The single draft PR: head = agent's git branch, base = seed default.
	if writer.openPR == nil {
		t.Fatal("OpenPR never called")
	}
	if !writer.openPR.Draft {
		t.Fatal("PR must be a draft")
	}
	if !strings.HasPrefix(writer.openPR.Branch, "bot/"+runID+"-") {
		t.Fatalf("branch = %q, want bot/%s-*", writer.openPR.Branch, runID)
	}
	if writer.openPR.Base != "main" {
		t.Fatalf("base = %q, want 'main' resolved from origin/HEAD", writer.openPR.Base)
	}
	if !strings.Contains(writer.openPR.Body, "bumped tar to 7.5.19") {
		t.Fatalf("PR body missing verdict summary:\n%s", writer.openPR.Body)
	}

	// The same envelope executed again dedupes via the ledger (no second PR).
	if recExec.captured == nil {
		t.Fatal("pipeline did not hand the executor an envelope")
	}
	res2, err := realExec.Execute(context.Background(), *recExec.captured, grant, worktree)
	if err != nil {
		t.Fatalf("second execute: %v", err)
	}
	if !res2.Deduped {
		t.Fatal("second execute with same envelope must dedupe")
	}
	if res2.GitHubRef != "acme/repo#42" {
		t.Fatalf("dedup ref = %q", res2.GitHubRef)
	}
}
