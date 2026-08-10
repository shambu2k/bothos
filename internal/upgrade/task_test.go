package upgrade

import (
	"os"
	"path/filepath"
	"testing"
)

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
