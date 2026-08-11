// Package review produces deterministic evidence for pull-request reviews.
package review

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/shambu2k/bothos/internal/scan"
)

// Finding is one deterministic observation about the granted pull-request diff.
type Finding struct {
	Rule     string
	Path     string
	Line     int
	Detail   string
	Evidence string
	Verified bool
}

type addedLine struct {
	Line int
	Text string
}

type diffFile struct {
	Path   string
	Header string
	Added  []addedLine
}

// All runs every deterministic review check against one shared zero-context diff.
func All(ctx context.Context, worktree string) ([]Finding, error) {
	files, err := loadDiff(ctx, worktree)
	if err != nil {
		return nil, err
	}

	findings := deniedPathFindings(files)
	findings = append(findings, secretFindings(files)...)

	dependencyFindings, dependencyInputs, err := dependencyDeltaFindings(ctx, worktree, files)
	if err != nil {
		return nil, err
	}
	findings = append(findings, dependencyFindings...)

	if len(dependencyInputs) > 0 {
		osv, err := osvDeltaFindings(ctx, worktree, dependencyInputs)
		if err != nil {
			return nil, err
		}
		findings = append(findings, osv...)
	}

	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Detail < b.Detail
	})
	return findings, nil
}

func loadDiff(ctx context.Context, worktree string) ([]diffFile, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "diff", "--unified=0", "refs/bothos/base..HEAD", "--")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("git diff: %w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("git diff: %w", err)
	}
	return parseDiff(out)
}

func parseDiff(raw []byte) ([]diffFile, error) {
	var files []diffFile
	var current *diffFile
	newLine := 0

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			path := diffHeaderPath(line)
			files = append(files, diffFile{Path: path, Header: line})
			current = &files[len(files)-1]
			newLine = 0
		case current == nil:
			continue
		case strings.HasPrefix(line, "+++ b/"):
			current.Path = strings.TrimPrefix(line, "+++ b/")
		case strings.HasPrefix(line, "@@ "):
			start, err := hunkHeadLine(line)
			if err != nil {
				return nil, err
			}
			newLine = start
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			current.Added = append(current.Added, addedLine{Line: newLine, Text: strings.TrimPrefix(line, "+")})
			newLine++
		case strings.HasPrefix(line, "-"):
			// Deleted lines do not advance the head line number.
		default:
			if newLine > 0 {
				newLine++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("parse git diff: %w", err)
	}
	for _, file := range files {
		if file.Path == "" {
			return nil, fmt.Errorf("parse git diff: missing path in %q", file.Header)
		}
	}
	return files, nil
}

func diffHeaderPath(header string) string {
	const marker = " b/"
	idx := strings.LastIndex(header, marker)
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(header[idx+len(marker):])
}

func hunkHeadLine(header string) (int, error) {
	fields := strings.Fields(header)
	if len(fields) < 3 || !strings.HasPrefix(fields[2], "+") {
		return 0, fmt.Errorf("parse git diff hunk: %q", header)
	}
	value := strings.TrimPrefix(fields[2], "+")
	if comma := strings.IndexByte(value, ','); comma >= 0 {
		value = value[:comma]
	}
	line, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse git diff hunk %q: %w", header, err)
	}
	return line, nil
}

func deniedPathFindings(files []diffFile) []Finding {
	var findings []Finding
	for _, file := range files {
		if file.Path != ".env" && !strings.HasSuffix(file.Path, ".key") && !strings.HasPrefix(file.Path, ".github/workflows/") {
			continue
		}
		line := 1
		evidence := file.Header
		if len(file.Added) > 0 {
			line = file.Added[0].Line
			evidence += "\n+" + file.Added[0].Text
		}
		findings = append(findings, Finding{
			Rule:     "denied_path",
			Path:     file.Path,
			Line:     line,
			Detail:   "changed denied path",
			Evidence: capBytes(evidence, 240),
			Verified: true,
		})
	}
	return findings
}

func dependencyDeltaFindings(ctx context.Context, worktree string, files []diffFile) ([]Finding, []string, error) {
	byPath := make(map[string]diffFile, len(files))
	var manifests, lockfiles []string
	for _, file := range files {
		byPath[file.Path] = file
		switch {
		case isSupportedManifest(file.Path):
			manifests = append(manifests, file.Path)
		case isRecognizedLockfile(file.Path):
			lockfiles = append(lockfiles, file.Path)
		}
	}
	sort.Strings(manifests)
	sort.Strings(lockfiles)

	inputs := append(append([]string(nil), manifests...), lockfiles...)
	if len(manifests) == 0 {
		if len(lockfiles) == 0 {
			return nil, nil, nil
		}
		path := lockfiles[0]
		return []Finding{{
			Rule:     "lockfile_only",
			Path:     path,
			Line:     firstLine(byPath[path]),
			Detail:   "lockfile changed without a supported manifest",
			Evidence: byPath[path].Header,
			Verified: true,
		}}, inputs, nil
	}

	var findings []Finding
	for _, path := range manifests {
		base, err := readBaseFile(ctx, worktree, path)
		if err != nil {
			return nil, nil, err
		}
		head, err := readHeadFile(worktree, path)
		if err != nil {
			return nil, nil, err
		}
		oldDeps, err := parseManifest(path, base)
		if err != nil {
			return nil, nil, fmt.Errorf("parse base %s: %w", path, err)
		}
		newDeps, err := parseManifest(path, head)
		if err != nil {
			return nil, nil, fmt.Errorf("parse head %s: %w", path, err)
		}
		for _, detail := range dependencyDetails(oldDeps, newDeps) {
			findings = append(findings, Finding{
				Rule:     "dependency_delta",
				Path:     path,
				Line:     firstLine(byPath[path]),
				Detail:   detail,
				Evidence: byPath[path].Header,
				Verified: true,
			})
		}
	}
	return findings, inputs, nil
}

func isSupportedManifest(path string) bool {
	base := filepath.Base(path)
	return base == "package.json" || base == "go.mod" || base == "requirements.txt"
}

func isRecognizedLockfile(path string) bool {
	base := filepath.Base(path)
	switch base {
	case "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "go.sum":
		return true
	default:
		return strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".lock")
	}
}

func readBaseFile(ctx context.Context, worktree, path string) ([]byte, error) {
	list := exec.CommandContext(ctx, "git", "-C", worktree, "ls-tree", "-r", "--name-only", "-z", "refs/bothos/base", "--", path)
	names, err := list.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-tree %s: %w", path, err)
	}
	if len(names) == 0 {
		return nil, nil
	}

	object := "refs/bothos/base:" + path
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "show", object)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git show %s: %w", path, err)
	}
	return out, nil
}

func readHeadFile(worktree, path string) ([]byte, error) {
	out, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(path)))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read head %s: %w", path, err)
	}
	return out, nil
}

func parseManifest(path string, data []byte) (map[string]string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]string{}, nil
	}
	switch filepath.Base(path) {
	case "package.json":
		return parsePackageJSON(data)
	case "go.mod":
		return parseGoMod(data), nil
	case "requirements.txt":
		return parseRequirements(data), nil
	default:
		return nil, fmt.Errorf("unsupported manifest")
	}
}

func parsePackageJSON(data []byte) (map[string]string, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	out := make(map[string]string)
	for _, section := range []string{"dependencies", "devDependencies", "peerDependencies", "optionalDependencies"} {
		raw, ok := root[section]
		if !ok {
			continue
		}
		var deps map[string]string
		if err := json.Unmarshal(raw, &deps); err != nil {
			return nil, fmt.Errorf("%s: %w", section, err)
		}
		for name, version := range deps {
			out[name] = version
		}
	}
	return out, nil
}

func parseGoMod(data []byte) map[string]string {
	out := make(map[string]string)
	inRequire := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "//", 2)[0])
		if line == "require (" {
			inRequire = true
			continue
		}
		if inRequire && line == ")" {
			inRequire = false
			continue
		}
		fields := strings.Fields(line)
		if !inRequire {
			if len(fields) == 3 && fields[0] == "require" {
				out[fields[1]] = fields[2]
			}
			continue
		}
		if len(fields) >= 2 {
			out[fields[0]] = fields[1]
		}
	}
	return out
}

func parseRequirements(data []byte) map[string]string {
	out := make(map[string]string)
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.SplitN(raw, "#", 2)[0])
		parts := strings.SplitN(line, "==", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(parts[0]))
		version := strings.TrimSpace(parts[1])
		if name != "" && version != "" {
			out[name] = version
		}
	}
	return out
}

func dependencyDetails(oldDeps, newDeps map[string]string) []string {
	keys := make(map[string]struct{}, len(oldDeps)+len(newDeps))
	for key := range oldDeps {
		keys[key] = struct{}{}
	}
	for key := range newDeps {
		keys[key] = struct{}{}
	}
	names := make([]string, 0, len(keys))
	for key := range keys {
		names = append(names, key)
	}
	sort.Strings(names)

	var details []string
	for _, name := range names {
		oldVersion, hadOld := oldDeps[name]
		newVersion, hasNew := newDeps[name]
		if hadOld && hasNew && oldVersion == newVersion {
			continue
		}
		switch {
		case !hadOld:
			details = append(details, fmt.Sprintf("%s: added -> %s", name, newVersion))
		case !hasNew:
			details = append(details, fmt.Sprintf("%s: %s -> removed", name, oldVersion))
		default:
			details = append(details, fmt.Sprintf("%s: %s -> %s", name, oldVersion, newVersion))
		}
	}
	return details
}

func osvDeltaFindings(ctx context.Context, worktree string, inputs []string) ([]Finding, error) {
	baseDir, err := os.MkdirTemp("", "bothos-review-base-")
	if err != nil {
		return nil, fmt.Errorf("create base tree: %w", err)
	}
	defer os.RemoveAll(baseDir)

	if err := materializeBase(ctx, worktree, baseDir); err != nil {
		return nil, err
	}
	baseFindings, err := scan.Run(ctx, baseDir, scan.StandardTools())
	if err != nil {
		return nil, fmt.Errorf("scan base: %w", err)
	}
	headFindings, err := scan.Run(ctx, worktree, scan.StandardTools())
	if err != nil {
		return nil, fmt.Errorf("scan head: %w", err)
	}

	type key struct {
		ecosystem string
		pkg       string
		advisory  string
	}
	baseSet := make(map[key]struct{}, len(baseFindings))
	for _, finding := range baseFindings {
		baseSet[key{finding.Ecosystem, finding.Package, finding.AdvisoryID}] = struct{}{}
	}
	headSet := make(map[key]scan.Finding, len(headFindings))
	for _, finding := range headFindings {
		k := key{finding.Ecosystem, finding.Package, finding.AdvisoryID}
		if _, present := baseSet[k]; !present {
			headSet[k] = finding
		}
	}
	keys := make([]key, 0, len(headSet))
	for k := range headSet {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ecosystem != keys[j].ecosystem {
			return keys[i].ecosystem < keys[j].ecosystem
		}
		if keys[i].pkg != keys[j].pkg {
			return keys[i].pkg < keys[j].pkg
		}
		return keys[i].advisory < keys[j].advisory
	})

	path := inputs[0]
	var out []Finding
	for _, k := range keys {
		finding := headSet[k]
		out = append(out, Finding{
			Rule:     "osv_delta",
			Path:     path,
			Line:     1,
			Detail:   fmt.Sprintf("%s@%s introduced %s", finding.Package, finding.CurrentVersion, finding.AdvisoryID),
			Evidence: "osv-scanner --format json .: " + finding.AdvisoryID,
			Verified: true,
		})
	}
	return out, nil
}

func materializeBase(ctx context.Context, worktree, destination string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "archive", "refs/bothos/base")
	archive, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git archive base: %w", err)
	}

	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read base archive: %w", err)
		}
		clean := filepath.Clean(filepath.FromSlash(header.Name))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe base archive path %q", header.Name)
		}
		path := filepath.Join(destination, clean)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return fmt.Errorf("create base directory: %w", err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("create base parent: %w", err)
			}
			file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return fmt.Errorf("create base file: %w", err)
			}
			_, copyErr := io.Copy(file, reader)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("extract base file: %w", copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close base file: %w", closeErr)
			}
		}
	}
}

func firstLine(file diffFile) int {
	if len(file.Added) == 0 {
		return 1
	}
	return file.Added[0].Line
}

func capBytes(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}
