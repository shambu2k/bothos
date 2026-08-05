package scan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeScanner writes a fixed JSON blob to stdout, standing in for a real
// scanner binary so Run's exec + parse wiring is tested without the tools.
const fakeScannerScript = `#!/bin/sh
cat <<'EOF'
{"results": [{"packages": [{"package": {"name": "leftpad", "version": "0.0.1", "ecosystem": "npm"},
  "vulnerabilities": [{"id": "GHSA-abc", "affected": [{"ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}, {"fixed": "0.0.2"}]}]}]}]}]}]}
EOF
`

// fakeFailing emits garbage / exits non-zero.
const fakeFailingScript = `#!/bin/sh
echo "boom" >&2
echo "not json"
exit 1
`

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestRunExecutesScannerAndParses(t *testing.T) {
	dir := t.TempDir()
	bin := writeScript(t, dir, "fake-osv", fakeScannerScript)

	got, err := Run(context.Background(), dir, []Tool{{
		Scanner: ScannerOSV, Bin: bin,
		Args:  func(dir string) []string { return nil },
		Parse: ParseOSV,
	}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	f := got[0]
	if f.Package != "leftpad" || f.TargetVersion != "0.0.2" || f.AdvisoryID != "GHSA-abc" {
		t.Fatalf("unexpected finding: %+v", f)
	}
}

func TestRunTreatsOsvExit1AsFindings(t *testing.T) {
	// osv-scanner exits 1 when it finds vulnerabilities; that is a successful
	// scan with findings, not a failure.
	dir := t.TempDir()
	bin := writeScript(t, dir, "fake-osv1", fakeScannerScript+"\nexit 1\n")

	got, err := Run(context.Background(), dir, []Tool{{
		Scanner: ScannerOSV, Bin: bin,
		Args:   func(dir string) []string { return nil },
		Parse:  ParseOSV,
		OKExit: func(code int) bool { return code == 0 || code == 1 },
	}})
	if err != nil {
		t.Fatalf("exit-1-with-findings must be treated as success: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
}

func TestRunPropagatesToolFailure(t *testing.T) {
	dir := t.TempDir()
	bin := writeScript(t, dir, "fake-bad", fakeFailingScript)

	_, err := Run(context.Background(), dir, []Tool{{
		Scanner: ScannerTrivy, Bin: bin,
		Args:  func(dir string) []string { return nil },
		Parse: ParseTrivy,
	}})
	if err == nil {
		t.Fatal("expected an error from a failing scanner")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected stderr in error, got: %v", err)
	}
}
