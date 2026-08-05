package scanjob

import (
	"context"
	"os"
	"testing"

	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/scan"
	"github.com/shambu2k/bothos/internal/testdb"
)

func writeFile(path, content string, mode os.FileMode) error {
	return os.WriteFile(path, []byte(content), mode)
}

// fakeScannerScript echoes osv-scanner-shaped JSON so the real parse path runs.
const fakeScannerScript = `#!/bin/sh
cat <<'EOF'
{"results": [{"packages": [{"package": {"name": "leftpad", "version": "0.0.1", "ecosystem": "npm"},
  "vulnerabilities": [{"id": "GHSA-abc", "affected": [{"ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}, {"fixed": "0.0.2"}]}]}]}]}]}]}
EOF
`

func TestRunScanClonesScansAndUpserts(t *testing.T) {
	ctx := context.Background()

	// real store against the test DB
	st, err := ledger.New(ctx, testdb.DSN(t))
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	testdb.Truncate(t, testdb.DSN(t), "runs", "findings", "river_job")
	st.InsertRun(ctx, ledger.Run{ID: "scan-1", RepoID: "shambu2k/repo", Trigger: "scheduled",
		ScopeKind: "scheduled", Grant: []byte(`{}`), Decision: "allow", Status: ledger.RunQueued})

	// fake clone: nothing to fetch (the fake scanner ignores the tree)
	fakeClone := func(ctx context.Context, dir, repo string) error { return nil }

	// fake scanner binary via scan.Run's tool path
	dir := t.TempDir()
	bin := dir + "/fake-osv"
	if err := writeFile(bin, fakeScannerScript, 0o755); err != nil {
		t.Fatalf("write fake scanner: %v", err)
	}
	tools := []scan.Tool{{Scanner: scan.ScannerOSV, Bin: bin,
		Args: func(dir string) []string { return nil }, Parse: scan.ParseOSV}}

	n, err := Run(ctx, Config{Clone: fakeClone, Tools: tools}, st, "shambu2k/repo", "scan-1")
	if err != nil {
		t.Fatalf("run scan: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 finding, got %d", n)
	}

	findings, err := st.Findings(ctx, "shambu2k/repo")
	if err != nil || len(findings) != 1 {
		t.Fatalf("no finding persisted: %v", err)
	}
	if findings[0].AdvisoryID != "GHSA-abc" || findings[0].TargetVersion != "0.0.2" {
		t.Fatalf("unexpected finding: %+v", findings[0])
	}
}
