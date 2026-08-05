package credstore

import (
	"context"
	"testing"

	"github.com/shambu2k/bothos/internal/intent"
)

func TestEnvResolveWrite(t *testing.T) {
	m := map[string]string{"GITHUB_WRITE_TOKEN": "abc", "GITHUB_WRITE_TOKEN_ACME": "acme-tok"}
	e := NewEnv(func(k string) string { return m[k] })

	if v, err := e.Resolve(context.Background(), "", intent.TokenContentsWrite); err != nil || v != "abc" {
		t.Fatalf("default write: v=%q err=%v", v, err)
	}
	if v, err := e.Resolve(context.Background(), "ACME", intent.TokenContentsWrite); err != nil || v != "acme-tok" {
		t.Fatalf("per-account write: v=%q err=%v", v, err)
	}
	if _, err := e.Resolve(context.Background(), "NOPE", intent.TokenContentsWrite); err == nil {
		t.Fatal("expected error for missing write token")
	}
}

func TestEnvResolveReadOnly(t *testing.T) {
	m := map[string]string{"GITHUB_READ_TOKEN": "rd"}
	e := NewEnv(func(k string) string { return m[k] })
	if v, err := e.Resolve(context.Background(), "", intent.TokenReadOnly); err != nil || v != "rd" {
		t.Fatalf("read: v=%q err=%v", v, err)
	}
}
