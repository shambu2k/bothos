package verifier

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shambu2k/bothos/internal/runtime"
)

// withFakeOSV puts the fake osv-scanner binary first on PATH for the test.
func withFakeOSV(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", abs+string(os.PathListSeparator)+os.Getenv("PATH"))
	return abs
}

// newRepo builds a real temp git repo with a committed baseline. Returns the
// worktree dir. Used for the uncommitted-changes and clean-pass checks.
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
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "base")
	return dir
}

const tarVulnJSON = `{"results":[{"packages":[{"package":{"name":"tar","version":"7.5.16","ecosystem":"npm"},"vulnerabilities":[{"id":"GHSA-abc"}]}]}]}`

func TestVerifyCleanPass(t *testing.T) {
	withFakeOSV(t)
	wt := newRepo(t)
	v := Verifier{} // default tools osv-only, no test command

	res := v.Verify(context.Background(), wt, []runtime.ClaimedFix{{Package: "tar", AdvisoryID: "GHSA-abc"}})
	if !res.Pass {
		t.Fatalf("clean repo should pass, got %+v", res.Failures)
	}
}

func TestVerifyUncommittedChanges(t *testing.T) {
	withFakeOSV(t)
	wt := newRepo(t)
	// Dirty the worktree with an untracked file.
	if err := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := (Verifier{}).Verify(context.Background(), wt, nil)
	if res.Pass {
		t.Fatal("dirty worktree must fail")
	}
	if !hasRule(res, RuleUncommitted) {
		t.Fatalf("want uncommitted_changes, got %+v", res.Failures)
	}
}

func TestVerifyVulnStillPresent(t *testing.T) {
	withFakeOSV(t)
	t.Setenv("FAKE_OSV_JSON", tarVulnJSON)
	wt := newRepo(t)

	// Claim a fix that is NOT actually present: the re-scan still finds tar.
	res := (Verifier{}).Verify(context.Background(), wt, []runtime.ClaimedFix{{Package: "tar", AdvisoryID: "GHSA-abc"}})
	if res.Pass {
		t.Fatal("claimed fix not present must fail")
	}
	found := false
	for _, f := range res.Failures {
		if f.Rule == RuleVulnPresent && strings.Contains(f.Detail, "tar") && strings.Contains(f.Detail, "GHSA-abc") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want vuln_still_present for tar, got %+v", res.Failures)
	}
}

func TestVerifyScannerErrorIsRed(t *testing.T) {
	withFakeOSV(t)
	t.Setenv("FAKE_OSV_EXIT", "2") // non-OK exit => scan fails
	wt := newRepo(t)

	res := (Verifier{}).Verify(context.Background(), wt, nil)
	if res.Pass {
		t.Fatal("scanner error must be red, not silently ignored")
	}
	if !hasRule(res, RuleScannerError) {
		t.Fatalf("want scanner_error, got %+v", res.Failures)
	}
}

func TestVerifyTestFailed(t *testing.T) {
	withFakeOSV(t)
	wt := newRepo(t)

	v := Verifier{TestCommand: func(string) string { return "exit 1" }}
	res := v.Verify(context.Background(), wt, nil)
	if res.Pass {
		t.Fatal("failing test must fail verification")
	}
	if !hasRule(res, RuleTestFailed) {
		t.Fatalf("want test_failed, got %+v", res.Failures)
	}
}

func TestSignatureStableAndDistinct(t *testing.T) {
	a := []Failure{{Rule: "vuln_still_present", Detail: "tar GHSA-abc still present at 7.5.16"}}
	b := sameFailureSet(a)
	c := []Failure{{Rule: "uncommitted_changes", Detail: "worktree dirty"}}
	if Signature(a) != Signature(b) {
		t.Fatal("same failure set must have same signature")
	}
	if Signature(a) == Signature(c) {
		t.Fatal("different failure sets must differ")
	}
}

func sameFailureSet(f []Failure) []Failure {
	out := make([]Failure, len(f))
	copy(out, f)
	return out
}

func TestFormatFeedbackMarksRepeatAndNew(t *testing.T) {
	prev := []Failure{{Rule: "vuln_still_present", Detail: "tar GHSA-abc still present at 7.5.16"}}
	cur := []Failure{
		{Rule: "vuln_still_present", Detail: "tar GHSA-abc still present at 7.5.16"}, // repeat
		{Rule: "test_failed", Detail: `"npm test" exited non-zero`},                  // new
	}
	out := FormatFeedback(prev, cur, 2, 3)
	if !strings.Contains(out, "[repeat]") {
		t.Errorf("repeat marker missing:\n%s", out)
	}
	if !strings.Contains(out, "[new]") {
		t.Errorf("new marker missing:\n%s", out)
	}
}

func TestFormatFeedbackCapsAt8000(t *testing.T) {
	big := strings.Repeat("x", 5000)
	cur := []Failure{{Rule: "test_failed", Detail: "fail", Snippet: big}}
	out := FormatFeedback(nil, cur, 1, 3)
	if len(out) > 8000 {
		t.Fatalf("feedback %d chars exceeds 8000 cap", len(out))
	}
	if !strings.Contains(out, "Please fix these") {
		t.Errorf("feedback missing instruction tail")
	}
}

func hasRule(res Result, rule string) bool {
	for _, f := range res.Failures {
		if f.Rule == rule {
			return true
		}
	}
	return false
}
