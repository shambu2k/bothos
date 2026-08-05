package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrUnknownRuntime is returned by New when the named runtime isn't registered.
var ErrUnknownRuntime = errors.New("unknown agent runtime")

// Factory builds a named AgentRuntime from a config blob. The config is
// runtime-specific (provider/model/keys live here, never on the interface or
// in RunInput), which is what keeps the AgentRuntime seam swappable.
type Factory func(ctx context.Context, cfg map[string]any) (AgentRuntime, error)

var (
	mu       sync.RWMutex
	registry = map[string]Factory{}
)

// Register makes a Runtime available under name. Called from an init() in the
// runtime's own package (e.g. internal/agent registers "pi").
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	registry[name] = f
}

// New instantiates the named runtime with cfg.
func New(name string, ctx context.Context, cfg map[string]any) (AgentRuntime, error) {
	mu.RLock()
	f, ok := registry[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownRuntime, name)
	}
	return f(ctx, cfg)
}
