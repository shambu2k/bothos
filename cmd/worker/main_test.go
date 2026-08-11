package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewSandboxChecksOutGrantedHeadDetachedWithBaseRef(t *testing.T) {
	origin, baseSHA, headSHA := seedReviewOrigin(t)
	sandboxer := newReviewSandboxerWithURL(func(string) string { return origin })
	sandbox, err := sandboxer(context.Background(), "acme/widget", 7, baseSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	worktree := sandbox.Worktree()
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(worktree)) })

	if got := workerGit(t, worktree, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD = %s, want %s", got, headSHA)
	}
	if got := workerGit(t, worktree, "rev-parse", "refs/bothos/base"); got != baseSHA {
		t.Fatalf("base ref = %s, want %s", got, baseSHA)
	}
	cmd := exec.Command("git", "-C", worktree, "symbolic-ref", "-q", "HEAD")
	if err := cmd.Run(); err == nil {
		t.Fatal("review checkout is attached to a branch")
	}
	if got := workerGit(t, worktree, "remote", "get-url", "origin"); got != origin || strings.Contains(got, "@github.com") {
		t.Fatalf("origin URL = %q", got)
	}
}

func TestReviewSandboxRejectsHeadSHAMismatch(t *testing.T) {
	origin, baseSHA, _ := seedReviewOrigin(t)
	sandboxer := newReviewSandboxerWithURL(func(string) string { return origin })
	if _, err := sandboxer(context.Background(), "acme/widget", 7, baseSHA, strings.Repeat("f", 40)); err == nil {
		t.Fatal("expected granted head mismatch error")
	}
}

func seedReviewOrigin(t *testing.T) (origin, baseSHA, headSHA string) {
	t.Helper()
	seed := t.TempDir()
	workerGit(t, seed, "init", "-q", "-b", "main")
	workerGit(t, seed, "config", "user.email", "worker@example.com")
	workerGit(t, seed, "config", "user.name", "worker")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workerGit(t, seed, "add", ".")
	workerGit(t, seed, "commit", "-qm", "base")
	baseSHA = workerGit(t, seed, "rev-parse", "HEAD")

	origin = filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v (%s)", err, out)
	}
	workerGit(t, seed, "remote", "add", "origin", origin)
	workerGit(t, seed, "push", "-q", "origin", "main")

	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("head\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workerGit(t, seed, "commit", "-qam", "head")
	headSHA = workerGit(t, seed, "rev-parse", "HEAD")
	workerGit(t, seed, "push", "-q", "origin", "HEAD:refs/pull/7/head")
	return origin, baseSHA, headSHA
}

func workerGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
