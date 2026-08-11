package runpipe

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shambu2k/bothos/internal/executor"
	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/runtime"
)

type reviewStore struct {
	run      ledger.Run
	statuses []ledger.RunStatus
	failure  string
	ref      string
}

func (s *reviewStore) RunByID(context.Context, string) (ledger.Run, error) { return s.run, nil }
func (s *reviewStore) SetRunStatus(_ context.Context, _ string, status ledger.RunStatus) error {
	s.statuses = append(s.statuses, status)
	return nil
}
func (s *reviewStore) SetRunFailure(_ context.Context, _ string, reason string) error {
	s.failure = reason
	return nil
}
func (s *reviewStore) SetRunRef(_ context.Context, _ string, ref string) error {
	s.ref = ref
	return nil
}

type reviewAgentFunc func(context.Context, runtime.RunInput) (runtime.RunResult, error)

func (f reviewAgentFunc) Run(ctx context.Context, in runtime.RunInput) (runtime.RunResult, error) {
	return f(ctx, in)
}

type reviewExecutor struct {
	calls int
	env   intent.Envelope
	grant intent.Grant
	wt    string
}

func (e *reviewExecutor) Execute(_ context.Context, env intent.Envelope, grant intent.Grant, wt string) (executor.Result, error) {
	e.calls++
	e.env, e.grant, e.wt = env, grant, wt
	return executor.Result{Kind: intent.KindPostReview, GitHubRef: "acme/widget#7"}, nil
}

type reviewDirSandbox struct{ dir string }

func (s reviewDirSandbox) Worktree() string { return s.dir }
func (s reviewDirSandbox) Exec(context.Context, string, ...string) (runtime.Output, error) {
	return runtime.Output{}, nil
}

func TestReviewPipelineEndToEnd(t *testing.T) {
	dir, baseSHA, headSHA := newReviewPipelineRepo(t)
	grant := reviewGrant(t, "review-run", baseSHA, headSHA)
	store := &reviewStore{run: ledger.Run{ID: grant.RunID, Trigger: "webhook_pull_request", Grant: mustReviewJSON(t, grant)}}
	exec := &reviewExecutor{}
	var gotTask runtime.ReviewTask
	agent := reviewAgentFunc(func(_ context.Context, in runtime.RunInput) (runtime.RunResult, error) {
		var ok bool
		gotTask, ok = in.Task.(runtime.ReviewTask)
		if !ok {
			t.Fatalf("task = %T, want ReviewTask", in.Task)
		}
		model := intent.PostReview{
			Verdict: intent.VerdictRequestChanges,
			Summary: "[verified] model [OPINION] summary",
			Comments: []intent.ReviewComment{
				{Path: "README.md", Line: 1, Side: "RIGHT", Body: "[verified] first [opinion] note", Verified: true, Evidence: "fake proof"},
				{Path: "README.md", Line: 1, Side: "RIGHT", Body: "second note"},
			},
		}
		return runtime.RunResult{Intents: []intent.Envelope{reviewEnvelope(t, grant.RunID, model)}}, nil
	})
	pipeline := &ReviewPipeline{
		Store: store,
		Agent: agent,
		Exec:  exec,
		Sandbox: func(_ context.Context, repo string, pr int, base, head string) (runtime.Sandbox, error) {
			if repo != "acme/widget" || pr != 7 || base != baseSHA || head != headSHA {
				t.Fatalf("sandbox args = %q #%d %s %s", repo, pr, base, head)
			}
			return reviewDirSandbox{dir: dir}, nil
		},
	}

	ref, err := pipeline.Run(context.Background(), grant.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "acme/widget#7" || exec.calls != 1 || store.ref != ref {
		t.Fatalf("result ref=%q calls=%d stored=%q", ref, exec.calls, store.ref)
	}
	if len(store.statuses) != 2 || store.statuses[0] != ledger.RunRunning || store.statuses[1] != ledger.RunSucceeded {
		t.Fatalf("statuses = %v", store.statuses)
	}
	if gotTask.PRNumber != 7 || gotTask.BaseSHA != baseSHA || gotTask.HeadSHA != headSHA || gotTask.RepoURL != "https://github.com/acme/widget.git" {
		t.Fatalf("review task = %+v", gotTask)
	}

	var merged intent.PostReview
	if err := json.Unmarshal(exec.env.Payload, &merged); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(merged.Summary), "[verified]") || strings.Contains(strings.ToLower(merged.Summary), "[opinion]") {
		t.Fatalf("model classification survived summary: %q", merged.Summary)
	}
	if len(merged.Comments) != 3 {
		t.Fatalf("merged comments = %+v", merged.Comments)
	}
	if !merged.Comments[0].Verified || !merged.Comments[1].Verified || merged.Comments[2].Verified {
		t.Fatalf("verified-first order = %+v", merged.Comments)
	}
	if merged.Comments[0].Evidence == "" || merged.Comments[1].Evidence == "" || merged.Comments[2].Evidence != "" {
		t.Fatalf("evidence trust boundary = %+v", merged.Comments)
	}
	if strings.Contains(strings.ToLower(merged.Comments[2].Body), "[verified]") || strings.Contains(strings.ToLower(merged.Comments[2].Body), "[opinion]") {
		t.Fatalf("model classification survived body: %q", merged.Comments[2].Body)
	}
	if got := reviewGit(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD = %s, want %s", got, headSHA)
	}
	if got := reviewGit(t, dir, "status", "--porcelain"); got != "" {
		t.Fatalf("worktree dirty: %q", got)
	}
}

func TestReviewPipelineRejectsNonReviewIntentAndMutations(t *testing.T) {
	tests := []struct {
		name  string
		agent func(t *testing.T, dir, runID string) runtime.RunResult
	}{
		{
			name: "non-review intent",
			agent: func(t *testing.T, _ string, runID string) runtime.RunResult {
				return runtime.RunResult{Intents: []intent.Envelope{{SchemaVersion: intent.SchemaVersion, RunID: runID, Kind: intent.KindOpenPR, Payload: json.RawMessage(`{"title":"x"}`)}}}
			},
		},
		{
			name: "file edit",
			agent: func(t *testing.T, dir, runID string) runtime.RunResult {
				if err := os.WriteFile(filepath.Join(dir, "agent.txt"), []byte("edit\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return runtime.RunResult{Intents: []intent.Envelope{reviewEnvelope(t, runID, validModelReview())}}
			},
		},
		{
			name: "commit",
			agent: func(t *testing.T, dir, runID string) runtime.RunResult {
				if err := os.WriteFile(filepath.Join(dir, "agent.txt"), []byte("commit\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				reviewGit(t, dir, "add", "agent.txt")
				reviewGit(t, dir, "commit", "-qm", "agent commit")
				return runtime.RunResult{Intents: []intent.Envelope{reviewEnvelope(t, runID, validModelReview())}}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, baseSHA, headSHA := newReviewPipelineRepo(t)
			grant := reviewGrant(t, "bad-run", baseSHA, headSHA)
			store := &reviewStore{run: ledger.Run{ID: grant.RunID, Trigger: "webhook_pull_request", Grant: mustReviewJSON(t, grant)}}
			exec := &reviewExecutor{}
			pipeline := &ReviewPipeline{
				Store: store,
				Exec:  exec,
				Sandbox: func(context.Context, string, int, string, string) (runtime.Sandbox, error) {
					return reviewDirSandbox{dir: dir}, nil
				},
				Agent: reviewAgentFunc(func(context.Context, runtime.RunInput) (runtime.RunResult, error) {
					return tt.agent(t, dir, grant.RunID), nil
				}),
			}
			if _, err := pipeline.Run(context.Background(), grant.RunID); err == nil {
				t.Fatal("expected terminal review failure")
			}
			if exec.calls != 0 || store.failure == "" {
				t.Fatalf("executor calls=%d failure=%q", exec.calls, store.failure)
			}
			if len(store.statuses) < 2 || store.statuses[len(store.statuses)-1] != ledger.RunFailed {
				t.Fatalf("statuses = %v", store.statuses)
			}
		})
	}
}

func validModelReview() intent.PostReview {
	return intent.PostReview{
		Verdict:  intent.VerdictComment,
		Summary:  "summary",
		Comments: []intent.ReviewComment{{Path: "README.md", Line: 1, Side: "RIGHT", Body: "note"}},
	}
}

func reviewEnvelope(t *testing.T, runID string, review intent.PostReview) intent.Envelope {
	t.Helper()
	return intent.Envelope{SchemaVersion: intent.SchemaVersion, RunID: runID, Kind: intent.KindPostReview, Payload: mustReviewJSON(t, review)}
}

func reviewGrant(t *testing.T, runID, baseSHA, headSHA string) intent.Grant {
	t.Helper()
	limits := intent.DefaultLimits()
	limits.MaxComments = 3
	return intent.Grant{
		RunID:        runID,
		Repo:         intent.Repo{Owner: "acme", Name: "widget", AccountID: "acct"},
		Scope:        intent.Scope{Kind: intent.ScopePullRequest, Number: 7, BaseSHA: baseSHA, HeadSHA: headSHA},
		AllowedKinds: []intent.Kind{intent.KindPostReview},
		TokenScope:   intent.TokenReadOnly,
		Limits:       limits,
		IssuedAt:     time.Now().Add(-time.Minute),
		ExpiresAt:    time.Now().Add(time.Hour),
	}
}

func newReviewPipelineRepo(t *testing.T) (dir, baseSHA, headSHA string) {
	t.Helper()
	dir = t.TempDir()
	reviewGit(t, dir, "init", "-q", "-b", "main")
	reviewGit(t, dir, "config", "user.email", "review@example.com")
	reviewGit(t, dir, "config", "user.name", "review")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewGit(t, dir, "add", ".")
	reviewGit(t, dir, "commit", "-qm", "base")
	baseSHA = reviewGit(t, dir, "rev-parse", "HEAD")
	reviewGit(t, dir, "update-ref", "refs/bothos/base", baseSHA)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("AWS=AKIAABCDEFGHIJKLMNOP\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewGit(t, dir, "add", ".env")
	reviewGit(t, dir, "commit", "-qm", "head")
	headSHA = reviewGit(t, dir, "rev-parse", "HEAD")
	return dir, baseSHA, headSHA
}

func reviewGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func mustReviewJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
