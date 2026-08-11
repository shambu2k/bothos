package credstore

import (
	"context"
	"testing"

	"github.com/shambu2k/bothos/internal/intent"
)

func TestEnvResolveWritePerAccountWins(t *testing.T) {
	// Both global and per-account set: the per-account override wins.
	m := map[string]string{"GITHUB_WRITE_TOKEN": "global", "GITHUB_WRITE_TOKEN_ACME": "acme-tok"}
	e := NewEnv(func(k string) string { return m[k] })
	if v, err := e.Resolve(context.Background(), "ACME", intent.TokenContentsWrite); err != nil || v != "acme-tok" {
		t.Fatalf("per-account write: v=%q err=%v (want acme-tok)", v, err)
	}
}

func TestEnvResolveWriteGlobalFallback(t *testing.T) {
	// No per-account token: fall back to the global GITHUB_WRITE_TOKEN.
	m := map[string]string{"GITHUB_WRITE_TOKEN": "global"}
	e := NewEnv(func(k string) string { return m[k] })
	if v, err := e.Resolve(context.Background(), "ACME", intent.TokenContentsWrite); err != nil || v != "global" {
		t.Fatalf("global fallback write: v=%q err=%v (want global)", v, err)
	}
}

func TestEnvResolveCommentPerAccountWins(t *testing.T) {
	m := map[string]string{
		"GITHUB_COMMENT_TOKEN":      "global-comment",
		"GITHUB_COMMENT_TOKEN_ACME": "acme-comment",
		"GITHUB_WRITE_TOKEN":        "contents-write",
	}
	e := NewEnv(func(k string) string { return m[k] })
	if v, err := e.Resolve(context.Background(), "ACME", intent.TokenIssuesWrite); err != nil || v != "acme-comment" {
		t.Fatalf("per-account comment: v=%q err=%v", v, err)
	}
}

func TestEnvResolveCommentGlobalFallbackWithoutContentsFallback(t *testing.T) {
	m := map[string]string{"GITHUB_COMMENT_TOKEN": "global-comment", "GITHUB_WRITE_TOKEN": "contents-write"}
	e := NewEnv(func(k string) string { return m[k] })
	if v, err := e.Resolve(context.Background(), "ACME", intent.TokenIssuesWrite); err != nil || v != "global-comment" {
		t.Fatalf("global comment: v=%q err=%v", v, err)
	}

	delete(m, "GITHUB_COMMENT_TOKEN")
	if _, err := e.Resolve(context.Background(), "ACME", intent.TokenIssuesWrite); err == nil {
		t.Fatal("issues-write unexpectedly fell back to contents-write token")
	}
}

func TestEnvResolveWriteDefaultAccountUsesGlobal(t *testing.T) {
	m := map[string]string{"GITHUB_WRITE_TOKEN": "global"}
	e := NewEnv(func(k string) string { return m[k] })
	if v, err := e.Resolve(context.Background(), "", intent.TokenContentsWrite); err != nil || v != "global" {
		t.Fatalf("default write: v=%q err=%v", v, err)
	}
}

func TestEnvResolveWriteMissingBothErrors(t *testing.T) {
	// Neither global nor per-account present: error, mentioning both.
	e := NewEnv(func(k string) string { return "" })
	if _, err := e.Resolve(context.Background(), "NOPE", intent.TokenContentsWrite); err == nil {
		t.Fatal("expected error when both write tokens are absent")
	}
	if _, err := e.Resolve(context.Background(), "", intent.TokenContentsWrite); err == nil {
		t.Fatal("expected error when global write token is absent")
	}
}

func TestEnvResolveReadOnly(t *testing.T) {
	m := map[string]string{"GITHUB_READ_TOKEN": "rd"}
	e := NewEnv(func(k string) string { return m[k] })
	if v, err := e.Resolve(context.Background(), "", intent.TokenReadOnly); err != nil || v != "rd" {
		t.Fatalf("read: v=%q err=%v", v, err)
	}
}
