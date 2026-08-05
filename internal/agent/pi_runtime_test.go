package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shambu2k/bothos/internal/runtime"
)

// fakeAdapter is a sh that reads the request line then emits one open_pr intent
// and exits 0 — exercises the full subprocess protocol without a model.
const fakeAdapter = `#!/bin/sh
IFS= read -r line
echo '{"type":"intent","intent":{"schema_version":1,"run_id":"r1","kind":"open_pr","payload":{"title":"t","draft":true,"worktree":"/tmp/x","topic":"upgrade-a-b"}}}'
exit 0
`

type stubSandbox struct{ wt string }

func (s stubSandbox) Exec(ctx context.Context, cmd string, args ...string) (runtime.Output, error) {
	return runtime.Output{}, nil
}
func (s stubSandbox) Worktree() string { return s.wt }

func TestPIRuntimeCollectsIntent(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-adapter")
	if err := os.WriteFile(script, []byte(fakeAdapter), 0o755); err != nil {
		t.Fatal(err)
	}
	rt, err := newPIRuntime(context.Background(), map[string]any{"node": "/bin/sh", "adapter": script})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res, err := rt.Run(context.Background(), runtime.RunInput{
		RunID:   "r1",
		Task:    runtime.UpgradeTask{Package: "adm-zip", CurrentVersion: "0.5.17", TargetVersion: "0.6.0"},
		Sandbox: stubSandbox{wt: dir},
		Limits:  runtime.Limits{MaxSeconds: 10 * time.Second},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Intents) != 1 {
		t.Fatalf("want 1 intent, got %d", len(res.Intents))
	}
	if res.Intents[0].Kind != "open_pr" || res.Intents[0].RunID != "r1" {
		t.Fatalf("unexpected intent: %+v", res.Intents[0])
	}
}

func TestPIRuntimeFailsWithoutIntent(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "failing-adapter")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nIFS= read -r line\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rt, err := newPIRuntime(context.Background(), map[string]any{"node": "/bin/sh", "adapter": script})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := rt.Run(context.Background(), runtime.RunInput{
		RunID: "r1", Sandbox: stubSandbox{wt: dir}, Limits: runtime.Limits{MaxSeconds: 10 * time.Second},
	}); err == nil {
		t.Fatal("expected error when adapter exits non-zero with no intent")
	}
}

func TestPIRuntimeMissingAdapter(t *testing.T) {
	rt, err := newPIRuntime(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := rt.Run(context.Background(), runtime.RunInput{
		RunID: "r1", Sandbox: stubSandbox{wt: t.TempDir()},
	}); err == nil {
		t.Fatal("expected config error for missing adapter path")
	}
}
