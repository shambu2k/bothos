package scan

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// RunRenovate runs `renovate --platform=local --dry-run --report-type=json`
// with cwd=dir and parses the renovate-report.json it writes there. It returns
// the available-update set. Unlike the stdout scanners, Renovate's output is a
// file, and it may exit non-zero for reasons that still produce a report, so
// the report file is the source of truth: we read it even on a non-zero exit.
// bin is the renovate executable (typically "renovate"); injectable for tests.
func RunRenovate(ctx context.Context, dir, bin string) ([]Update, error) {
	cmd := exec.CommandContext(ctx, bin,
		"--platform=local", "--dry-run", "--report-type=json",
		"--log-level=error", "--token=local")
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	runErr := cmd.Run()

	report, rerr := os.ReadFile(filepath.Join(dir, "renovate-report.json"))
	if rerr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("renovate: %w: %s", runErr, errBuf.String())
		}
		return nil, fmt.Errorf("renovate report: %w", rerr)
	}
	updates, perr := ParseRenovate(report)
	if perr != nil {
		return nil, perr
	}
	return updates, nil
}
