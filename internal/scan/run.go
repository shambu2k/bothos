package scan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// Tool describes how to run one scanner and parse its JSON output.
type Tool struct {
	Scanner Scanner
	Bin     string              // executable to spawn
	Args    func(dir string) []string
	Parse   func(out []byte) ([]Finding, error)
	// OKExit reports whether an exit code means "the scan ran" (not a hard
	// failure). Default is code==0. osv-scanner returns 1 when it finds
	// vulnerabilities, which is a successful scan with findings.
	OKExit func(code int) bool
}

func (t Tool) exitOK(code int) bool {
	if t.OKExit == nil {
		return code == 0
	}
	return t.OKExit(code)
}

// StandardTools returns the scanners that run by default. osv-scanner is the
// Phase 1 workhorse: standalone (reads lockfiles/go.mod itself, no toolchain
// in the container) and multi-ecosystem. govulncheck needs the go toolchain in
// the image and trivy needs its binary, so both are deliberately not in the
// default set for now — add them once the runtime has their deps.
func StandardTools() []Tool {
	return []Tool{
		{Scanner: ScannerOSV, Bin: "osv-scanner",
			Args:   func(dir string) []string { return []string{"--format", "json", dir} },
			Parse:  ParseOSV,
			OKExit: func(code int) bool { return code == 0 || code == 1 }, // 1 = findings found
		},
	}
}

// Run executes each tool against dir (cwd = dir) and returns the union of
// normalised findings. It stops at the first tool that fails to run or parse.
func Run(ctx context.Context, dir string, tools []Tool) ([]Finding, error) {
	var all []Finding
	for _, tl := range tools {
		cmd := exec.CommandContext(ctx, tl.Bin, tl.Args(dir)...)
		cmd.Dir = dir
		var out, errBuf bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			var exErr *exec.ExitError
			if errors.As(err, &exErr) && tl.exitOK(exErr.ExitCode()) {
				// tool succeeded and signalled findings with a non-zero exit
				// (e.g. osv-scanner exits 1 when it finds vulns); parse below
			} else {
				return nil, fmt.Errorf("%s: %w: %s", tl.Scanner, err, errBuf.String())
			}
		}
		fs, err := tl.Parse(out.Bytes())
		if err != nil {
			return nil, fmt.Errorf("%s parse: %w", tl.Scanner, err)
		}
		all = append(all, fs...)
	}
	return all, nil
}
