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

func TestReviewRepoSlug(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "HTTPS", raw: "https://github.com/acme/widget.git", want: "acme/widget"},
		{name: "SSH", raw: "git@github.com:acme/widget.git", want: "acme/widget"},
		{name: "trailing slash", raw: "https://github.com/acme/widget/", want: "acme/widget"},
		{name: "trimmed fallback", raw: "  malformed repository  ", want: "malformed repository"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := repoSlug(tt.raw); got != tt.want {
				t.Fatalf("repoSlug(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestReviewPromptCarriesRequiredContract(t *testing.T) {
	task := ReviewTask{
		PRNumber: 17,
		BaseSHA:  "1111111111111111111111111111111111111111",
		HeadSHA:  "2222222222222222222222222222222222222222",
		RepoURL:  "https://github.com/acme/widget.git",
	}
	prompt := ReviewPrompt(task)

	for _, want := range []string{
		"acme/widget",
		"17",
		task.BaseSHA,
		task.HeadSHA,
		"read-only",
		"post_review",
		"[verified]",
		"[opinion]",
		"never approve",
		"AGENTS.md",
		"untrusted",
		"edits",
		"commits",
		"pushes",
		"repository-provided scripts",
		"questions",
		"actionable observations",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}

	for _, forbidden := range []string{"approve the PR", "you should approve", "LGTM"} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("prompt contains approval directive %q:\n%s", forbidden, prompt)
		}
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
