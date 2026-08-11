package review

import (
	"path/filepath"
	"regexp"
	"strings"
)

type secretRule struct {
	detail  string
	pattern *regexp.Regexp
}

var addedSecretRules = []secretRule{
	{detail: "possible AWS access key", pattern: regexp.MustCompile(`AKIA[A-Z0-9]{16}`)},
	{detail: "possible GitHub token", pattern: regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]{20,}`)},
	{detail: "possible GitHub fine-grained token", pattern: regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`)},
	{detail: "possible OpenAI API key", pattern: regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`)},
	{detail: "PEM private material", pattern: regexp.MustCompile(`-----BEGIN [A-Z0-9 ]+-----`)},
}

func secretFindings(files []diffFile) []Finding {
	var findings []Finding
	for _, file := range files {
		for _, line := range file.Added {
			for _, rule := range addedSecretRules {
				if !rule.pattern.MatchString(line.Text) {
					continue
				}
				findings = append(findings, Finding{
					Rule:     "secret",
					Path:     file.Path,
					Line:     line.Line,
					Detail:   rule.detail,
					Evidence: capBytes("+"+line.Text, 240),
					Verified: true,
				})
			}
			if strings.Contains(line.Text, "http://") && allowsBareHTTPCheck(file.Path) {
				findings = append(findings, Finding{
					Rule:     "secret",
					Path:     file.Path,
					Line:     line.Line,
					Detail:   "bare HTTP URL in configuration",
					Evidence: capBytes("+"+line.Text, 240),
					Verified: true,
				})
			}
		}
	}
	return findings
}

func allowsBareHTTPCheck(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	if base == ".env" || strings.Contains(base, "config") {
		return true
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".json", ".yaml", ".yml", ".toml", ".ini", ".conf", ".config":
		return true
	default:
		return false
	}
}
