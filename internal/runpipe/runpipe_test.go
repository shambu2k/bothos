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

type fakeStore struct {
	run            ledger.Run
	last           ledger.RunStatus
	failedReason   string
	failureCalled  bool
	ref            string
	usageModel     string
	usageTokensIn  int
	usageTokensOut int
	usageCostUSD   float64
}

func (s *fakeStore) RunByID(ctx context.Context, id string) (ledger.Run, error) { return s.run, nil }
func (s *fakeStore) SetRunStatus(ctx context.Context, id string, st ledger.RunStatus) error {
	s.last = st
	return nil
}
func (s *fakeStore) SetRunFailure(ctx context.Context, id, reason string) error {
	s.failureCalled = true
	s.failedReason = reason
	return nil
}
func (s *fakeStore) SetRunNeedsInput(ctx context.Context, id, reason string) error {
	s.last = ledger.RunNeedsInput
	s.failedReason = reason
	return nil
}
func (s *fakeStore) SetRunRef(ctx context.Context, id, ref string) error {
	s.ref = ref
	return nil
}
func (s *fakeStore) SetRunUsage(ctx context.Context, id, model string, tokensIn, tokensOut int, costUSD float64) error {
	s.usageModel = model
	s.usageTokensIn = tokensIn
	s.usageTokensOut = tokensOut
	s.usageCostUSD = costUSD
	return nil
}

type fakeSandbox struct{ wt string }

func (s fakeSandbox) Worktree() string { return s.wt }
func (s fakeSandbox) Exec(ctx context.Context, c string, a ...string) (runtime.Output, error) {
	return runtime.Output{}, nil
}

type fakeAgent struct {
	intents []intent.Envelope
	verdict *runtime.Verdict
}

func (a fakeAgent) Run(ctx context.Context, in runtime.RunInput) (runtime.RunResult, error) {
	return runtime.RunResult{Intents: a.intents, Verdict: a.verdict}, nil
}

type fakeExec struct {
	result       executor.Result
	lastWorktree string
	env          intent.Envelope
	calls        int
}

func (e *fakeExec) Execute(ctx context.Context, env intent.Envelope, g intent.Grant, worktree string) (executor.Result, error) {
	e.calls++
	e.env = env
	e.lastWorktree = worktree
	return e.result, nil
}

func openPREnvelope(t *testing.T, runID string) intent.Envelope {
	t.Helper()
	payload, _ := json.Marshal(intent.OpenPR{Title: "x", Body: "y", Draft: true})
	return intent.Envelope{SchemaVersion: int(intent.SchemaVersion), RunID: runID, Kind: intent.KindOpenPR, Payload: payload}
}

func TestPipelineHappyPath(t *testing.T) {
	grant := intent.Grant{
		RunID:     "r1",
		Repo:      intent.Repo{Owner: "acme", Name: "repo"},
		Scope:     intent.Scope{Kind: intent.ScopeScheduled, BaseRef: "main"},
		IssuedAt:  time.Now().Add(-time.Minute),
		ExpiresAt: time.Now().Add(45 * time.Minute),
	}
	grantJSON, _ := json.Marshal(grant)
	metaJSON, _ := json.Marshal(UpgradeMeta{Scope: "security", BaseRef: "main"})

	st := &fakeStore{run: ledger.Run{ID: "r1", Grant: grantJSON, Meta: metaJSON, Decision: "allow"}}
	sb := fakeSandbox{wt: "/tmp/fake-worktree"}
	ex := &fakeExec{result: executor.Result{GitHubRef: "acme/repo#9"}}

	p := &Pipeline{
		Store:   st,
		Agent:   fakeAgent{intents: []intent.Envelope{openPREnvelope(t, "r1")}},
		Exec:    ex,
		Sandbox: func(ctx context.Context, repo string) (runtime.Sandbox, error) { return sb, nil },
	}

	ref, err := p.Run(context.Background(), "r1")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if ref != "acme/repo#9" {
		t.Fatalf("ref: got %q", ref)
	}
	if st.last != ledger.RunSucceeded {
		t.Fatalf("expected succeeded, got %q", st.last)
	}
	// The executor received the sandbox worktree out-of-band.
	if ex.lastWorktree != "/tmp/fake-worktree" {
		t.Fatalf("executor worktree = %q, want /tmp/fake-worktree", ex.lastWorktree)
	}
}

func TestPipelineFailsWithoutIntent(t *testing.T) {
	grantJSON, _ := json.Marshal(intent.Grant{
		Repo:      intent.Repo{Owner: "acme", Name: "repo"},
		IssuedAt:  time.Now().Add(-time.Minute),
		ExpiresAt: time.Now().Add(45 * time.Minute),
	})
	metaJSON, _ := json.Marshal(UpgradeMeta{Scope: "security", BaseRef: "main"})
	st := &fakeStore{run: ledger.Run{ID: "r2", Grant: grantJSON, Meta: metaJSON}}
	p := &Pipeline{
		Store: st,
		Agent: fakeAgent{}, // no intents, no verdict
		Exec:  &fakeExec{},
		Sandbox: func(ctx context.Context, r string) (runtime.Sandbox, error) {
			return fakeSandbox{"/tmp/x"}, nil
		},
	}
	if _, err := p.Run(context.Background(), "r2"); err == nil {
		t.Fatal("expected error when no open_pr intent is produced")
	}
	if st.last != ledger.RunFailed {
		t.Fatalf("expected failed, got %q", st.last)
	}
	if !st.failureCalled {
		t.Fatal("SetRunFailure must record the reason for every failure (diagnostics)")
	}
	if !strings.Contains(st.failedReason, "no open_pr intent produced") {
		t.Fatalf("failure reason = %q", st.failedReason)
	}
}

func TestPipelineBlockedStandDownIsTerminal(t *testing.T) {
	// An agent that stands down (blocked verdict, no open_pr intent) must
	// record the failure reason and return nil — a stated run, NOT a River
	// retry.
	grantJSON, _ := json.Marshal(intent.Grant{
		Repo:      intent.Repo{Owner: "acme", Name: "repo"},
		IssuedAt:  time.Now().Add(-time.Minute),
		ExpiresAt: time.Now().Add(45 * time.Minute),
	})
	metaJSON, _ := json.Marshal(UpgradeMeta{Scope: "security", BaseRef: "main"})
	st := &fakeStore{run: ledger.Run{ID: "r3", Grant: grantJSON, Meta: metaJSON}}
	ex := &fakeExec{}
	p := &Pipeline{
		Store: st,
		Agent: fakeAgent{verdict: &runtime.Verdict{Status: runtime.VerdictBlocked, Summary: "cannot migrate: API removed"}},
		Exec:  ex,
		Sandbox: func(ctx context.Context, r string) (runtime.Sandbox, error) {
			return fakeSandbox{"/tmp/x"}, nil
		},
	}
	ref, err := p.Run(context.Background(), "r3")
	if err != nil {
		t.Fatalf("stand-down must not error: %v", err)
	}
	if ref != "" {
		t.Fatalf("stand-down ref = %q, want empty", ref)
	}
	if !st.failureCalled {
		t.Fatal("SetRunFailure not called on stand-down")
	}
	if !strings.Contains(st.failedReason, "agent blocked: cannot migrate") {
		t.Fatalf("failure reason = %q", st.failedReason)
	}
	if ex.lastWorktree != "" {
		t.Fatal("executor must not be called on stand-down")
	}
}

// rejectAgent fails the test if the runtime is ever invoked, proving fail-fast:
// a stale grant must be rejected before the agent (and its token spend) starts.
type rejectAgent struct{}

func (rejectAgent) Run(ctx context.Context, in runtime.RunInput) (runtime.RunResult, error) {
	panic("agent must not run: grant was already expired")
}

// TestPipelineExpiryFailsFastAndSkipsAgent: a stale grant must fail the run
// BEFORE the agent starts (no wasted token spend), deterministically via the
// injected clock, and record the reason as a failed run.
func TestPipelineExpiryFailsFastAndSkipsAgent(t *testing.T) {
	expires := time.Unix(1_700_003_600, 0)
	issued := time.Unix(1_700_000_000, 0)
	grantJSON, _ := json.Marshal(intent.Grant{
		Repo:      intent.Repo{Owner: "acme", Name: "repo"},
		IssuedAt:  issued,
		ExpiresAt: expires, // fixed past expiry
	})
	metaJSON, _ := json.Marshal(UpgradeMeta{Scope: "security", BaseRef: "main"})
	st := &fakeStore{run: ledger.Run{ID: "r4", Grant: grantJSON, Meta: metaJSON, Decision: "allow"}}
	sandboxCalled := false
	p := &Pipeline{
		Store: st,
		Agent: rejectAgent{}, // would panic if invoked
		Exec:  &fakeExec{},
		Sandbox: func(ctx context.Context, r string) (runtime.Sandbox, error) {
			sandboxCalled = true
			return fakeSandbox{"/tmp/x"}, nil
		},
		Now: func() time.Time { return time.Unix(1_700_007_200, 0) }, // after expires
	}
	ref, err := p.Run(context.Background(), "r4")
	if err == nil {
		t.Fatal("expected error for expired grant")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("error missing 'expired': %v", err)
	}
	if sandboxCalled {
		t.Fatal("sandbox must not be created for an expired grant")
	}
	if ref != "" {
		t.Fatalf("expected no ref, got %q", ref)
	}
	if st.last != ledger.RunFailed {
		t.Fatalf("expected RunFailed, got %q", st.last)
	}
	if !st.failureCalled || !strings.Contains(st.failedReason, "expired") {
		t.Fatalf("failure reason = %q", st.failedReason)
	}
}

// TestPipelineRunsWithinGrantWindow: with Now pinned before ExpiresAt, the
// expiry gate must not trip and the run proceeds to a normal success.
func TestPipelineRunsBeforeExpiry(t *testing.T) {
	expires := time.Unix(1_700_100_000, 0)
	grantJSON, _ := json.Marshal(intent.Grant{
		Repo:      intent.Repo{Owner: "acme", Name: "repo"},
		IssuedAt:  time.Unix(1_700_000_000, 0),
		ExpiresAt: expires,
	})
	metaJSON, _ := json.Marshal(UpgradeMeta{Scope: "security", BaseRef: "main"})
	st := &fakeStore{run: ledger.Run{ID: "r5", Grant: grantJSON, Meta: metaJSON, Decision: "allow"}}
	sb := fakeSandbox{wt: "/tmp/fake-worktree"}
	ex := &fakeExec{result: executor.Result{GitHubRef: "acme/repo#9"}}
	p := &Pipeline{
		Store:   st,
		Agent:   fakeAgent{intents: []intent.Envelope{openPREnvelope(t, "r5")}},
		Exec:    ex,
		Sandbox: func(ctx context.Context, repo string) (runtime.Sandbox, error) { return sb, nil },
		Now:     func() time.Time { return time.Unix(1_700_000_000, 0) }, // before expires
	}
	ref, err := p.Run(context.Background(), "r5")
	if err != nil {
		t.Fatalf("run with unexpired grant must not error: %v", err)
	}
	if ref != "acme/repo#9" || ex.calls != 1 || st.last != ledger.RunSucceeded {
		t.Fatalf("ref=%q execCalls=%d status=%q", ref, ex.calls, st.last)
	}
}
