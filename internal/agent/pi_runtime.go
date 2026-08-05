package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"

	"github.com/shambu2k/bothos/internal/runtime"
)

// piRuntime is the AgentRuntime backed by the PI Node sidecar. It launches the
// adapter as a subprocess (cwd = the sandbox worktree), sends one Request line,
// and reads JSON-lines Events until the adapter exits, collecting intents.
type piRuntime struct {
	node    string // executable that runs the adapter (default "node")
	adapter string // absolute path to the adapter script
}

// Factory-registered constructor: runtime.Register("pi", newPIRuntime).
func newPIRuntime(ctx context.Context, cfg map[string]any) (runtime.AgentRuntime, error) {
	r := &piRuntime{node: "node"}
	if v, ok := cfg["node"].(string); ok && v != "" {
		r.node = v
	}
	if v, ok := cfg["adapter"].(string); ok && v != "" {
		r.adapter = v
	}
	return r, nil
}

func init() { runtime.Register("pi", newPIRuntime) }

func (r *piRuntime) Run(ctx context.Context, in runtime.RunInput) (runtime.RunResult, error) {
	worktree := in.Sandbox.Worktree()
	if r.adapter == "" {
		return runtime.RunResult{}, fmt.Errorf("pi adapter path not configured")
	}
	if in.Limits.MaxSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, in.Limits.MaxSeconds)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, r.node, r.adapter)
	cmd.Dir = worktree
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return runtime.RunResult{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runtime.RunResult{}, err
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		return runtime.RunResult{}, fmt.Errorf("start pi adapter: %w", err)
	}
	enc := json.NewEncoder(stdin)
	if err := enc.Encode(Request{RunID: in.RunID, Task: in.Task, Worktree: worktree, Limits: in.Limits}); err != nil {
		_ = cmd.Process.Kill()
		return runtime.RunResult{}, fmt.Errorf("write request: %w (adapter exited early: %s)", err, errBuf.String())
	}
	_ = stdin.Close()

	var result runtime.RunResult
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		ev, err := ParseEvent(sc.Bytes())
		if err != nil {
			log.Printf("pi adapter junk line: %v", err)
			continue
		}
		switch ev.Type {
		case EventIntent:
			if ev.Intent != nil {
				result.Intents = append(result.Intents, *ev.Intent)
			}
		case EventTool:
			log.Printf("run %s: adapter tool %q (%s)", in.RunID, ev.Tool, ev.Msg)
		case EventError:
			log.Printf("run %s: adapter error: %s", in.RunID, ev.Msg)
		}
	}
	if err := cmd.Wait(); err != nil {
		// A non-zero exit with no intents is a hard failure; with intents we
		// trust the worktree + executor validation downstream.
		if len(result.Intents) == 0 {
			return result, fmt.Errorf("pi adapter failed: %w: %s", err, errBuf.String())
		}
	}
	return result, nil
}

var _ runtime.AgentRuntime = (*piRuntime)(nil)
