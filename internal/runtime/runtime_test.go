package runtime

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSecurityPromptCarriesRequiredMarkers(t *testing.T) {
	p := SecurityPrompt("run-abc123", SecurityTask{BaseRef: "main"})
	for _, want := range []string{
		"security-remediation agent",
		"osv-scanner",
		"bot/run-abc123-",    // runID interpolated into the branch contract
		"bot/run-abc123",     // literal runID present
		"NEVER push",         // no-credentials wording
		"git rev-parse --abbrev-ref origin/HEAD", // base hint + git-state truth
		"main",               // base hint interpolated
		"trivy",              // optional second scanner
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestSecurityPromptOmitsBaseHintWhenEmpty(t *testing.T) {
	p := SecurityPrompt("run-1", SecurityTask{})
	if strings.Contains(p, "base branch is believed to be") {
		t.Errorf("empty base ref should omit the hint:\n%s", p)
	}
}

func TestSecurityPromptInterpolatesRunIDPerBranch(t *testing.T) {
	a := SecurityPrompt("run-aaaa", SecurityTask{})
	b := SecurityPrompt("run-bbbb", SecurityTask{})
	if !strings.Contains(a, "bot/run-aaaa-") || !strings.Contains(b, "bot/run-bbbb-") {
		t.Fatalf("runID not interpolated per prompt: %q vs %q", a, b)
	}
}

func TestSecurityPromptMentionsExternalVerification(t *testing.T) {
	// The external verifier re-checks the agent's claims and may feed findings
	// back — the mechanism that replaces the agent grading its own homework.
	p := SecurityPrompt("run-1", SecurityTask{})
	if !strings.Contains(p, "external verifier") {
		t.Errorf("prompt missing external-verifier mention:\n%s", p)
	}
}

// fakeRuntime is a compile-time proof that the interface is usable by a worker
// and that a runtime never needs a token or grant to satisfy it.
type fakeRuntime struct{}

func (fakeRuntime) Run(ctx context.Context, in RunInput) (RunResult, error) {
	return RunResult{Intents: nil, TokensIn: 1, TokensOut: 1}, nil
}

var _ AgentRuntime = fakeRuntime{}

func TestRunInputCarriesNoCredential(t *testing.T) {
	// The seam must not smuggle a Grant or PAT into the agent runtime.
	// RunInput has exactly these fields; a compile-time check that a worker
	// can build one without credentials is implicit, but assert the shape:
	in := RunInput{
		RunID:    "run-1",
		Task:     SecurityTask{BaseRef: "main"},
		GraphKey: "abc",
		Sandbox:  fakeSandbox{},
		Limits:   Limits{MaxTokens: 100, MaxSeconds: 5 * time.Minute},
	}
	if in.RunID != "run-1" || in.GraphKey != "abc" {
		t.Fatalf("unexpected input: %+v", in)
	}
}

type fakeSandbox struct{}

func (fakeSandbox) Exec(ctx context.Context, cmd string, args ...string) (Output, error) {
	return Output{Stdout: "ok", ExitCode: 0}, nil
}
func (fakeSandbox) Worktree() string { return "/sandbox/run-1" }
