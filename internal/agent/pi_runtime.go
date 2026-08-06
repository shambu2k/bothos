package agent

import (
	"context"

	"github.com/shambu2k/bothos/internal/runtime"
)

// newPIRuntime is the factory-registered constructor (runtime.Register("pi", …)).
// Config keys (registry config + gitignored env, never on RunInput):
//
//	pi          — path to the "pi" CLI binary (default "pi" on PATH)
//	model       — provider/id (default openrouter/deepseek/deepseek-v4-flash-0731)
//	session_dir — root for persistent per-run sessions (default "")
//	approve     — bool; pass --approve to trust the throwaway sandbox
func newPIRuntime(ctx context.Context, cfg map[string]any) (runtime.AgentRuntime, error) {
	r := NewPIRPC("", "", "", false)
	if v, ok := cfg["pi"].(string); ok && v != "" {
		r.piBin = v
	}
	if v, ok := cfg["model"].(string); ok && v != "" {
		r.model = v
	}
	if v, ok := cfg["session_dir"].(string); ok && v != "" {
		r.sessionDir = v
	}
	if v, ok := cfg["approve"].(bool); ok && v {
		r.approve = true
	}
	return r, nil
}

func init() { runtime.Register("pi", newPIRuntime) }
