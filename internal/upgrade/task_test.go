package upgrade

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shambu2k/bothos/internal/ledger"
)

func TestUpgradeTaskFromCandidate(t *testing.T) {
	c := ledger.Candidate{
		RepoID: "acme/repo", Package: "adm-zip", CurrentVersion: "0.5.17",
		TargetVersion: "0.6.0", Severity: "HIGH", AdvisoryID: "GHSA-abc",
	}
	task := UpgradeTaskFromCandidate(c, "npm test")
	if task.Package != "adm-zip" || task.CurrentVersion != "0.5.17" || task.TargetVersion != "0.6.0" {
		t.Fatalf("task fields mismatch: %+v", task)
	}
	if task.TestCommand != "npm test" {
		t.Fatalf("test command not passed: %q", task.TestCommand)
	}
}

func TestTestCommandFor(t *testing.T) {
	dir := t.TempDir()

	if got := TestCommandFor(dir); got != "" {
		t.Fatalf("empty repo should have no test command, got %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := TestCommandFor(dir); got != "go test ./..." {
		t.Fatalf("go repo: got %q", got)
	}
}
