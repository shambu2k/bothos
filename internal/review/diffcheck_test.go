package review

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAllFindsDeniedPathsAndAddedSecrets(t *testing.T) {
	dir := newReviewRepo(t, map[string]string{
		"removed.txt": "AKIAABCDEFGHIJKLMNOP\n",
		"config.yaml": "endpoint: https://example.com\n",
	})
	commitReviewHead(t, dir, map[string]string{
		"removed.txt":              "",
		"config.yaml":              "endpoint: https://example.com\ninsecure: http://example.com\n",
		".env":                     "AWS=AKIAABCDEFGHIJKLMNOP\n",
		"signing.key":              "-----BEGIN PRIVATE KEY-----\n",
		".github/workflows/ci.yml": "token: ghp_abcdefghijklmnopqrstuvwxyz0123456789\n",
		"tokens.json":              "fine: github_pat_abcdefghijklmnopqrstuvwxyz0123456789\n",
		"notes.txt":                "openai: sk-abcdefghijklmnopqrstuvwxyz0123456789\n",
	})
	withFakeOSV(t, dir)

	got, err := All(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{".env", "signing.key", ".github/workflows/ci.yml"} {
		if !hasFinding(got, "denied_path", path, "") {
			t.Errorf("missing denied-path finding for %s: %+v", path, got)
		}
	}
	for _, fragment := range []string{"AKIA", "BEGIN PRIVATE KEY", "ghp_", "github_pat_", "sk-", "http://"} {
		if !hasFinding(got, "secret", "", fragment) {
			t.Errorf("missing secret family %q: %+v", fragment, got)
		}
	}
	for _, finding := range got {
		if !finding.Verified {
			t.Errorf("finding is not verified: %+v", finding)
		}
		if strings.Contains(finding.Evidence, "https://example.com") && finding.Rule == "secret" {
			t.Errorf("HTTPS was treated as a secret: %+v", finding)
		}
		if finding.Path == "removed.txt" {
			t.Errorf("removed line produced a finding: %+v", finding)
		}
	}
	if !slices.IsSortedFunc(got, compareFinding) {
		t.Fatalf("findings are not stable-sorted: %+v", got)
	}
}

func TestAllTruncatesSecretEvidence(t *testing.T) {
	dir := newReviewRepo(t, map[string]string{"README.md": "base\n"})
	commitReviewHead(t, dir, map[string]string{"notes.txt": "sk-" + strings.Repeat("x", 400) + "\n"})
	withFakeOSV(t, dir)

	got, err := All(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range got {
		if finding.Rule == "secret" && len([]byte(finding.Evidence)) > 240 {
			t.Fatalf("secret evidence is %d bytes, want <= 240", len([]byte(finding.Evidence)))
		}
	}
}

func TestAllReportsManifestDependencyDeltas(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		base       string
		head       string
		wantDetail []string
	}{
		{
			name:       "package json",
			path:       "package.json",
			base:       `{"dependencies":{"left":"1.0.0","gone":"2.0.0"}}`,
			head:       `{"dependencies":{"left":"1.1.0","new":"3.0.0"}}`,
			wantDetail: []string{"left: 1.0.0 -> 1.1.0", "gone: 2.0.0 -> removed", "new: added -> 3.0.0"},
		},
		{
			name:       "go mod",
			path:       "go.mod",
			base:       "module example.com/x\nrequire example.com/a v1.0.0\n",
			head:       "module example.com/x\nrequire (\n\texample.com/a v1.1.0\n\texample.com/b v2.0.0\n)\n",
			wantDetail: []string{"example.com/a: v1.0.0 -> v1.1.0", "example.com/b: added -> v2.0.0"},
		},
		{
			name:       "requirements",
			path:       "requirements.txt",
			base:       "Django==4.2.0\nold==1.0\n",
			head:       "django == 4.2.1\nnew==2.0\n",
			wantDetail: []string{"django: 4.2.0 -> 4.2.1", "new: added -> 2.0", "old: 1.0 -> removed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := newReviewRepo(t, map[string]string{tt.path: tt.base})
			commitReviewHead(t, dir, map[string]string{tt.path: tt.head})
			withFakeOSV(t, dir)

			got, err := All(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, detail := range tt.wantDetail {
				if !hasFinding(got, "dependency_delta", tt.path, detail) {
					t.Errorf("missing %q: %+v", detail, got)
				}
			}
		})
	}
}

func TestAllReportsLockfileOnlyAndSkipsOSVForUnchangedInputs(t *testing.T) {
	t.Run("lockfile only", func(t *testing.T) {
		dir := newReviewRepo(t, map[string]string{"package-lock.json": "{}\n"})
		commitReviewHead(t, dir, map[string]string{"package-lock.json": "{\"lockfileVersion\":3}\n"})
		log := withFakeOSV(t, dir)

		got, err := All(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		if !hasFinding(got, "lockfile_only", "package-lock.json", "") {
			t.Fatalf("missing lockfile-only finding: %+v", got)
		}
		if invocations := readOptional(t, log); len(strings.Fields(invocations)) != 2 {
			t.Fatalf("OSV invocations = %q, want base and head", invocations)
		}
	})

	t.Run("unrelated diff", func(t *testing.T) {
		dir := newReviewRepo(t, map[string]string{"README.md": "base\n"})
		commitReviewHead(t, dir, map[string]string{"README.md": "head\n"})
		log := withFakeOSV(t, dir)

		if _, err := All(context.Background(), dir); err != nil {
			t.Fatal(err)
		}
		if invocations := readOptional(t, log); invocations != "" {
			t.Fatalf("OSV unexpectedly ran: %q", invocations)
		}
	})
}

func TestAllReportsOnlyNewOSVVulnerabilities(t *testing.T) {
	dir := newReviewRepo(t, map[string]string{"package-lock.json": "{}\n"})
	commitReviewHead(t, dir, map[string]string{"package-lock.json": "{\"lockfileVersion\":3}\n"})
	withFakeOSV(t, dir)
	t.Setenv("FAKE_OSV_BASE_JSON", osvJSON(
		osvEntry("npm", "shared", "1.0.0", "GHSA-shared"),
		osvEntry("npm", "old", "1.0.0", "GHSA-old"),
	))
	t.Setenv("FAKE_OSV_HEAD_JSON", osvJSON(
		osvEntry("npm", "shared", "1.0.0", "GHSA-shared"),
		osvEntry("npm", "new", "2.0.0", "GHSA-new"),
		osvEntry("npm", "new", "2.0.0", "GHSA-new"),
	))

	got, err := All(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var osv []Finding
	for _, finding := range got {
		if finding.Rule == "osv_delta" {
			osv = append(osv, finding)
		}
	}
	if len(osv) != 1 || osv[0].Detail != "new@2.0.0 introduced GHSA-new" || !strings.Contains(osv[0].Evidence, "osv-scanner --format json .") {
		t.Fatalf("unexpected OSV delta: %+v", osv)
	}
}

func TestAllReturnsScannerFailure(t *testing.T) {
	dir := newReviewRepo(t, map[string]string{"go.mod": "module example.com/x\n"})
	commitReviewHead(t, dir, map[string]string{"go.mod": "module example.com/x\nrequire example.com/a v1.0.0\n"})
	withFakeOSV(t, dir)
	t.Setenv("FAKE_OSV_EXIT", "2")

	if _, err := All(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "osv-scanner") {
		t.Fatalf("All error = %v, want scanner failure", err)
	}
}

func newReviewRepo(t *testing.T, base map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "review@example.com")
	runGit(t, dir, "config", "user.name", "review")
	writeReviewFiles(t, dir, base)
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-qm", "base")
	runGit(t, dir, "update-ref", "refs/bothos/base", "HEAD")
	return dir
}

func commitReviewHead(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	writeReviewFiles(t, dir, files)
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "head")
}

func writeReviewFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if content == "" {
			if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func withFakeOSV(t *testing.T, headDir string) string {
	t.Helper()
	binDir := t.TempDir()
	log := filepath.Join(binDir, "calls")
	script := `#!/bin/sh
set -u
for arg do dir="$arg"; done
printf 'x\n' >> "$FAKE_OSV_LOG"
if [ -n "${FAKE_OSV_EXIT:-}" ]; then
  echo "osv-scanner: boom" >&2
  exit "$FAKE_OSV_EXIT"
fi
if [ "$dir" = "$FAKE_OSV_HEAD_DIR" ]; then json="${FAKE_OSV_HEAD_JSON:-}"; else json="${FAKE_OSV_BASE_JSON:-}"; fi
if [ -z "$json" ]; then json='{"results":[]}'; fi
printf '%s\n' "$json"
case "$json" in *'"vulnerabilities":['*) exit 1;; esac
exit 0
`
	path := filepath.Join(binDir, "osv-scanner")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_OSV_LOG", log)
	t.Setenv("FAKE_OSV_HEAD_DIR", headDir)
	return log
}

func hasFinding(findings []Finding, rule, path, fragment string) bool {
	for _, finding := range findings {
		if finding.Rule == rule && (path == "" || finding.Path == path) &&
			(fragment == "" || strings.Contains(finding.Detail, fragment) || strings.Contains(finding.Evidence, fragment)) {
			return true
		}
	}
	return false
}

func compareFinding(a, b Finding) int {
	ak := fmt.Sprintf("%s\x00%s\x00%010d\x00%s", a.Rule, a.Path, a.Line, a.Detail)
	bk := fmt.Sprintf("%s\x00%s\x00%010d\x00%s", b.Rule, b.Path, b.Line, b.Detail)
	return strings.Compare(ak, bk)
}

func readOptional(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func osvEntry(ecosystem, name, version, advisory string) string {
	return fmt.Sprintf(`{"package":{"name":%q,"version":%q,"ecosystem":%q},"vulnerabilities":[{"id":%q}]}`, name, version, ecosystem, advisory)
}

func osvJSON(entries ...string) string {
	return `{"results":[{"packages":[` + strings.Join(entries, ",") + `]}]}`
}
