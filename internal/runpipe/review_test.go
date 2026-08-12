package runpipe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v69/github"
	"github.com/shambu2k/bothos/internal/executor"
	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/review"
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
func (s *reviewStore) SetRunNeedsInput(_ context.Context, _ string, reason string) error {
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
	grant.Manual = true
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
	acknowledged := false
	pipeline := &ReviewPipeline{
		Store: store,
		Agent: agent,
		Exec:  exec,
		Acknowledge: func(context.Context, intent.Grant) error {
			acknowledged = true
			return context.DeadlineExceeded
		},
		Sandbox: func(_ context.Context, repo string, pr int, base, head string) (runtime.Sandbox, error) {
			if repo != "acme/widget" || pr != 7 || base != baseSHA || head != headSHA {
				t.Fatalf("sandbox args = %q #%d %s %s", repo, pr, base, head)
			}
			if !acknowledged {
				t.Fatal("sandbox started before manual acknowledgement")
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

type persistentReviewLedger struct {
	intents  map[string]string
	comment  int64
	hasEntry bool
}

func (l *persistentReviewLedger) Lookup(_ context.Context, key string) (string, bool, error) {
	ref, ok := l.intents[key]
	return ref, ok, nil
}
func (l *persistentReviewLedger) Record(_ context.Context, key, _ string, ref string) error {
	l.intents[key] = ref
	return nil
}
func (l *persistentReviewLedger) ReviewCommentID(context.Context, string, int) (int64, bool, error) {
	return l.comment, l.hasEntry, nil
}
func (l *persistentReviewLedger) UpsertReviewComment(_ context.Context, _ string, _ int, id int64) error {
	l.comment, l.hasEntry = id, true
	return nil
}

type reviewCredentialStore struct{}

func (reviewCredentialStore) Resolve(context.Context, string, intent.TokenScope) (string, error) {
	return "comment-token", nil
}

type unusedReviewDiff struct{}

func (unusedReviewDiff) FromWorktree(context.Context, string, string) (intent.Diff, error) {
	return intent.Diff{}, nil
}

func TestReviewPipelineReusesAcknowledgementAcrossTwoHeads(t *testing.T) {
	var commentBody string
	creates, edits, inline := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user":
			fmt.Fprint(w, `{"login":"bothos-bot"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/7/comments"):
			if commentBody == "" {
				fmt.Fprint(w, `[]`)
			} else {
				fmt.Fprintf(w, `[{"id":700,"body":%q,"user":{"login":"bothos-bot"}}]`, commentBody)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/7/comments") && !strings.Contains(r.URL.Path, "/reviews"):
			var payload struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			creates++
			commentBody = payload.Body
			fmt.Fprint(w, `{"id":700}`)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/pulls/7/comments"):
			// inline PR review comment: return a created comment.
			inline++
			fmt.Fprint(w, `{"id":701}`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/issues/comments/700"):
			var payload struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			edits++
			commentBody = payload.Body
			fmt.Fprint(w, `{"id":700}`)
		default:
			t.Fatalf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	baseURL, _ := url.Parse(server.URL + "/")
	writer := executor.NewGitHubWriter(func(string) *github.Client {
		client := github.NewClient(server.Client())
		client.BaseURL = baseURL
		return client
	})
	execLedger := &persistentReviewLedger{intents: map[string]string{}}
	exec := executor.NewExecutor(reviewCredentialStore{}, execLedger, writer, unusedReviewDiff{}, time.Now)

	dir, baseSHA, firstHead := newReviewPipelineRepo(t)
	firstGrant := reviewGrant(t, "persistent-1", baseSHA, firstHead)
	firstGrant.Manual = true
	store := &reviewStore{run: ledger.Run{ID: firstGrant.RunID, Trigger: "webhook_pull_request", Grant: mustReviewJSON(t, firstGrant)}}
	agent := reviewAgentFunc(func(_ context.Context, input runtime.RunInput) (runtime.RunResult, error) {
		model := validModelReview()
		model.Summary = input.RunID
		return runtime.RunResult{Intents: []intent.Envelope{reviewEnvelope(t, input.RunID, model)}}, nil
	})
	pipeline := &ReviewPipeline{
		Store:       store,
		Agent:       agent,
		Exec:        exec,
		Acknowledge: exec.AcknowledgeReview,
		Sandbox: func(context.Context, string, int, string, string) (runtime.Sandbox, error) {
			return reviewDirSandbox{dir: dir}, nil
		},
		Checks: func(context.Context, string) ([]review.Finding, error) { return nil, nil },
	}
	if _, err := pipeline.Run(context.Background(), firstGrant.RunID); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("second head\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewGit(t, dir, "commit", "-qam", "second head")
	secondHead := reviewGit(t, dir, "rev-parse", "HEAD")
	secondGrant := firstGrant
	secondGrant.RunID = "persistent-2"
	secondGrant.Manual = false
	secondGrant.Scope.HeadSHA = secondHead
	store.run = ledger.Run{ID: secondGrant.RunID, Trigger: "webhook_pull_request", Grant: mustReviewJSON(t, secondGrant)}
	if _, err := pipeline.Run(context.Background(), secondGrant.RunID); err != nil {
		t.Fatal(err)
	}

	if creates != 1 || edits != 2 || execLedger.comment != 700 {
		t.Fatalf("creates=%d edits=%d mapping=%d", creates, edits, execLedger.comment)
	}
	if !strings.Contains(commentBody, "persistent-2") || strings.Contains(commentBody, "Review queued") {
		t.Fatalf("final comment body = %q", commentBody)
	}
}
