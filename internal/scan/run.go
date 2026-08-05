package scan

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// Tool describes how to run one scanner and parse its JSON output.
type Tool struct {
	Scanner Scanner
	Bin     string              // executable to spawn
	Args    func(dir string) []string
	Parse   func(out []byte) ([]Finding, error)
}

// StandardTools returns the scanners that run by default. osv-scanner is the
// Phase 1 workhorse: standalone (reads lockfiles/go.mod itself, no toolchain
// in the container) and multi-ecosystem. govulncheck needs the go toolchain in
// the image and trivy needs its binary, so both are deliberately not in the
// default set for now — add them once the runtime has their deps.
func StandardTools() []Tool {
	return []Tool{
		{Scanner: ScannerOSV, Bin: "osv-scanner",
			Args: func(dir string) []string { return []string{"--format", "json", dir} }, Parse: ParseOSV},
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
			return nil, fmt.Errorf("%s: %w: %s", tl.Scanner, err, errBuf.String())
		}
		fs, err := tl.Parse(out.Bytes())
		if err != nil {
			return nil, fmt.Errorf("%s parse: %w", tl.Scanner, err)
		}
		all = append(all, fs...)
	}
	return all, nil
}
