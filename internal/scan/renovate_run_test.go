package scan

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeRenovate writes a report file into cwd. Version A exits 0, version B
// exits 1 but still writes the report.
const fakeRenovateOK = `#!/bin/sh
cat > "$PWD/renovate-report.json" <<'EOF'
{"repositories":[{"updates":[
  {"depName":"express","currentVersion":"4.17.0","newVersion":"4.19.0","updateType":"minor","packageFile":"package.json"}
]}]}
EOF
exit 0
`
const fakeRenovateExit1 = `#!/bin/sh
cat > "$PWD/renovate-report.json" <<'EOF'
{"repositories":[{"updates":[
  {"depName":"tar","currentVersion":"7.5.16","newVersion":"7.5.19","updateType":"patch","packageFile":"package.json"}
]}]}
EOF
exit 1
`

func TestRunRenovate(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-renovate")
	if err := os.WriteFile(bin, []byte(fakeRenovateOK), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := RunRenovate(context.Background(), dir, bin)
	if err != nil {
		t.Fatalf("run renovate: %v", err)
	}
	if len(got) != 1 || got[0].Package != "express" || got[0].TargetVersion != "4.19.0" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestRunRenovateNonZeroExitStillParsesReport(t *testing.T) {
	// Renovate can exit non-zero and still write a report; the report is truth.
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-renovate1")
	if err := os.WriteFile(bin, []byte(fakeRenovateExit1), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := RunRenovate(context.Background(), dir, bin)
	if err != nil {
		t.Fatalf("run renovate: %v", err)
	}
	if len(got) != 1 || got[0].Package != "tar" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestRunRenovateMissingReport(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "no-report")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := RunRenovate(context.Background(), dir, bin); err == nil {
		t.Fatal("expected error when no report file is written")
	}
}
