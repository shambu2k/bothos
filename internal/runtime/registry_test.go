package runtime

import (
	"context"
	"errors"
	"testing"
)

func TestRuntimeRegistry(t *testing.T) {
	// ensure the registry is fresh for an isolated test
	registry = map[string]Factory{}

	Register("fake", func(ctx context.Context, cfg map[string]any) (AgentRuntime, error) {
		cfg["sealed"] = true
		return fakeRuntime{}, nil
	})

	if _, err := New("fake", context.Background(), map[string]any{"a": 1}); err != nil {
		t.Fatalf("New fake: %v", err)
	}
	if _, err := New("pi", context.Background(), nil); !errors.Is(err, ErrUnknownRuntime) {
		t.Fatalf("unknown runtime should error, got %v", err)
	}
}
