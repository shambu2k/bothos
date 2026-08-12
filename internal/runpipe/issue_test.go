package runpipe

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shambu2k/bothos/internal/executor"
	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/runtime"
)

func issueGrant(t *testing.T, runID string) intent.Grant {
	t.Helper()
	return intent.Grant{
		RunID:        runID,
		Repo:         intent.Repo{Owner: "acme", Name: "widget", AccountID: "acme"},
		Scope:        intent.Scope{Kind: intent.ScopeIssue, Number: 42, BaseRef: "main"},
		AllowedKinds: []intent.Kind{intent.KindOpenPR, intent.KindPostComment},
		TokenScope:   intent.TokenContentsWrite,
		Limits:       intent.DefaultLimits(),
		IssuedAt:     time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour),
	}
}

func issueRun(t *testing.T, grant intent.Grant) ledger.Run {
	t.Helper()
	g, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	m, err := json.Marshal(runtime.IssueTask{IssueNumber: 42, Title: "Repair login", Body: "Empty passwords are accepted.", RepoURL: "https://github.com/acme/widget.git", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	return ledger.Run{ID: grant.RunID, Trigger: "webhook_issue_labeled", Grant: g, Meta: m, Decision: "allow"}
}

func TestIssuePipelineOpensExactlyOneDraftPR(t *testing.T) {
	grant := issueGrant(t, "issue-1")
	store := &fakeStore{run: issueRun(t, grant)}
	exec := &fakeExec{result: executor.Result{Kind: intent.KindOpenPR, GitHubRef: "acme/widget#9"}}
	var task runtime.IssueTask
	pipeline := &IssuePipeline{
		Store: store,
		Agent: fakeAgentFunc(func(_ context.Context, in runtime.RunInput) (runtime.RunResult, error) {
			var ok bool
			task, ok = in.Task.(runtime.IssueTask)
			if !ok {
				t.Fatalf("task = %T", in.Task)
			}
			return runtime.RunResult{Intents: []intent.Envelope{openPREnvelope(t, grant.RunID)}}, nil
		}),
		Exec:    exec,
		Sandbox: func(context.Context, string) (runtime.Sandbox, error) { return fakeSandbox{wt: "/tmp/issue"}, nil },
	}

	ref, err := pipeline.Run(context.Background(), grant.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "acme/widget#9" || store.ref != ref || store.last != ledger.RunSucceeded {
		t.Fatalf("ref=%q stored=%q status=%q", ref, store.ref, store.last)
	}
	if exec.calls != 1 || exec.env.Kind != intent.KindOpenPR {
		t.Fatalf("executor calls=%d env=%+v", exec.calls, exec.env)
	}
	var pr intent.OpenPR
	if err := json.Unmarshal(exec.env.Payload, &pr); err != nil {
		t.Fatal(err)
	}
	if !pr.Draft {
		t.Fatalf("issue PR must be draft: %+v", pr)
	}
	if task.IssueNumber != 42 || task.Title != "Repair login" || task.Body != "Empty passwords are accepted." {
		t.Fatalf("issue task=%+v", task)
	}
}

func TestIssuePipelineDerivesTargetingFromGrant(t *testing.T) {
	grant := issueGrant(t, "issue-target")
	storedMeta, err := json.Marshal(runtime.IssueTask{
		IssueNumber: 999, RepoURL: "https://github.com/attacker/other.git", BaseRef: "attacker-branch",
		Title: "normal", Body: "untrusted text",
	})
	if err != nil {
		t.Fatal(err)
	}
	storedRun := issueRun(t, grant)
	storedRun.Meta = storedMeta
	store := &fakeStore{run: storedRun}
	var got runtime.IssueTask
	pipeline := &IssuePipeline{
		Store: store,
		Agent: fakeAgentFunc(func(_ context.Context, in runtime.RunInput) (runtime.RunResult, error) {
			got = in.Task.(runtime.IssueTask)
			return runtime.RunResult{Verdict: &runtime.Verdict{Status: runtime.VerdictBlocked, Summary: "need answer"}}, nil
		}),
		Exec:    &fakeExec{result: executor.Result{GitHubRef: "acme/widget#42"}},
		Sandbox: func(context.Context, string) (runtime.Sandbox, error) { return fakeSandbox{wt: "/tmp/issue"}, nil },
	}
	if _, err := pipeline.Run(context.Background(), grant.RunID); err != nil {
		t.Fatal(err)
	}
	if got.IssueNumber != 42 || got.RepoURL != "https://github.com/acme/widget.git" || got.BaseRef != "main" {
		t.Fatalf("targeting must come from grant, got %+v", got)
	}
}

func TestIssuePipelineBlockedPostsTrustedHandoff(t *testing.T) {
	grant := issueGrant(t, "issue-blocked")
	store := &fakeStore{run: issueRun(t, grant)}
	exec := &fakeExec{result: executor.Result{Kind: intent.KindPostComment, GitHubRef: "acme/widget#42"}}
	pipeline := &IssuePipeline{
		Store: store,
		Agent: fakeAgentFunc(func(context.Context, runtime.RunInput) (runtime.RunResult, error) {
			return runtime.RunResult{Verdict: &runtime.Verdict{Status: runtime.VerdictBlocked, Summary: "The API owner must choose which v2 endpoint replaces v1."}}, nil
		}),
		Exec:    exec,
		Sandbox: func(context.Context, string) (runtime.Sandbox, error) { return fakeSandbox{wt: "/tmp/issue"}, nil },
	}

	ref, err := pipeline.Run(context.Background(), grant.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "acme/widget#42" || store.ref != ref || store.last != ledger.RunNeedsInput {
		t.Fatalf("ref=%q stored=%q status=%q", ref, store.ref, store.last)
	}
	if exec.calls != 1 || exec.env.Kind != intent.KindPostComment {
		t.Fatalf("executor calls=%d env=%+v", exec.calls, exec.env)
	}
	var handoff intent.PostComment
	if err := json.Unmarshal(exec.env.Payload, &handoff); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Bothos is blocked", "No pull request was opened", "one answer", "v2 endpoint"} {
		if !strings.Contains(handoff.Body, want) {
			t.Errorf("handoff missing %q: %q", want, handoff.Body)
		}
	}
	if !strings.Contains(store.failedReason, "agent blocked") {
		t.Fatalf("blocker reason=%q", store.failedReason)
	}
}

type fakeAgentFunc func(context.Context, runtime.RunInput) (runtime.RunResult, error)

func (f fakeAgentFunc) Run(ctx context.Context, in runtime.RunInput) (runtime.RunResult, error) {
	return f(ctx, in)
}
