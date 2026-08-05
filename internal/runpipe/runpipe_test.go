package runpipe

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/shambu2k/bothos/internal/executor"
	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/runtime"
)

type fakeStore struct {
	run  ledger.Run
	last ledger.RunStatus
}

func (s *fakeStore) RunByID(ctx context.Context, id string) (ledger.Run, error) { return s.run, nil }
func (s *fakeStore) SetRunStatus(ctx context.Context, id string, st ledger.RunStatus) error {
	s.last = st
	return nil
}

type fakeSandbox struct{ wt string }

func (s fakeSandbox) Worktree() string                             { return s.wt }
func (s fakeSandbox) Exec(ctx context.Context, c string, a ...string) (runtime.Output, error) {
	return runtime.Output{}, nil
}

type fakeAgent struct{ intents []intent.Envelope }

func (a fakeAgent) Run(ctx context.Context, in runtime.RunInput) (runtime.RunResult, error) {
	return runtime.RunResult{Intents: a.intents}, nil
}

type fakeExec struct{ result executor.Result }

func (e fakeExec) Execute(ctx context.Context, env intent.Envelope, g intent.Grant) (executor.Result, error) {
	return e.result, nil
}

func TestPipelineHappyPath(t *testing.T) {
	grant := intent.Grant{
		RunID: "r1",
		Repo:  intent.Repo{Owner: "acme", Name: "repo"},
		Scope: intent.Scope{Kind: intent.ScopeScheduled, BaseRef: "main"},
	}
	grantJSON, _ := json.Marshal(grant)
	metaJSON, _ := json.Marshal(UpgradeMeta{Package: "adm-zip", From: "0.5.17", To: "0.6.0", AdvisoryID: "GHSA-a"})

	st := &fakeStore{run: ledger.Run{ID: "r1", Grant: grantJSON, Meta: metaJSON, Decision: "allow"}}
	dir := "/tmp/fake-worktree"
	sb := fakeSandbox{wt: dir}

	p := &Pipeline{
		Store:   st,
		Agent:   fakeAgent{intents: []intent.Envelope{{RunID: "r1", Kind: "open_pr", Payload: json.RawMessage(`{"topic":"upgrade-adm-zip-0.6.0"}`)}}},
		Exec:    fakeExec{result: executor.Result{GitHubRef: "acme/repo#9"}},
		Sandbox: func(ctx context.Context, repo, branch, base string) (runtime.Sandbox, error) { return sb, nil },
		ReTest:  func(ctx context.Context, w, c string) error { return nil },
		Commit:  func(ctx context.Context, w, b string) error { return nil },
	}

	ref, err := p.Run(context.Background(), "r1", "upgrade-adm-zip-0.6.0")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if ref != "acme/repo#9" {
		t.Fatalf("ref: got %q", ref)
	}
	if st.last != ledger.RunSucceeded {
		t.Fatalf("expected succeeded, got %q", st.last)
	}
}

func TestPipelineFailsWithoutIntent(t *testing.T) {
	grantJSON, _ := json.Marshal(intent.Grant{Repo: intent.Repo{Owner: "acme", Name: "repo"}})
	metaJSON, _ := json.Marshal(UpgradeMeta{Package: "adm-zip", From: "0.5.17", To: "0.6.0"})
	st := &fakeStore{run: ledger.Run{ID: "r2", Grant: grantJSON, Meta: metaJSON}}
	p := &Pipeline{
		Store:   st,
		Agent:   fakeAgent{}, // no intents
		Exec:    fakeExec{},
		Sandbox: func(ctx context.Context, r, b, base string) (runtime.Sandbox, error) { return fakeSandbox{"/tmp/x"}, nil },
	}
	if _, err := p.Run(context.Background(), "r2", "topic"); err == nil {
		t.Fatal("expected error when no open_pr intent is produced")
	}
	if st.last != ledger.RunFailed {
		t.Fatalf("expected failed, got %q", st.last)
	}
}
