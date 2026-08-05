package scan

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// RunRenovate runs Renovate in dry-run against a repo and parses the
// renovate-report.json it writes, returning the available-update set.
//
// We use --platform=github with the readonly GITHUB_READ_TOKEN (contents:read)
// rather than --platform=local: Renovate's --platform=local branch-update phase
// hard-fails ("Cannot sync git when platform=local") and never writes a report.
// --dry-run means it computes updates only — no PRs, no remote writes, no
// drafts. bin is the renovate executable; injectable for tests.
func RunRenovate(ctx context.Context, dir, repo, bin string) ([]Update, error) {
	cmd := exec.CommandContext(ctx, bin,
		"--platform=github", "--dry-run", "--report-type=json", "--onboarding=false", repo)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LOG_LEVEL=error", "RENOVATE_TOKEN="+os.Getenv("GITHUB_READ_TOKEN"))
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	runErr := cmd.Run()

	report, rerr := os.ReadFile(filepath.Join(dir, "renovate-report.json"))
	if rerr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("renovate: %w: %s", runErr, errBuf.String())
		}
		if report, rerr = findReportAnywhere(dir); rerr != nil {
			return nil, fmt.Errorf("renovate report: %w", rerr)
		}
	}
	updates, perr := ParseRenovate(report)
	if perr != nil {
		return nil, perr
	}
	return updates, nil
}

// findReportAnywhere looks for renovate-report.json under dir (Renovate can
// write it in different working directories depending on version).
func findReportAnywhere(dir string) ([]byte, error) {
	var found []byte
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && info.Name() == "renovate-report.json" {
			found, _ = os.ReadFile(p)
			return filepath.SkipAll
		}
		return nil
	})
	if found == nil {
		return nil, os.ErrNotExist
	}
	return found, nil
}
