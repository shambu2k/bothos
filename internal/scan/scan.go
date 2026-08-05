// Package scan runs deterministic security scanners (osv-scanner, trivy,
// govulncheck) against a repo and normalises their output into findings. No
// LLM is involved: the output is a table of facts, never prose.
package scan

// Scanner identifies a supported scanner tool.
type Scanner string

const (
	ScannerOSV   Scanner = "osv-scanner"
	ScannerTrivy Scanner = "trivy"
	ScannerGovuln Scanner = "govulncheck"
)

// Finding is one vulnerability record normalised from a scanner's JSON output.
// It maps 1:1 to the ledger's findings table.
type Finding struct {
	RepoID         string
	Scanner        Scanner
	Ecosystem      string
	Package        string
	CurrentVersion string // installed version, may be empty when unknown
	TargetVersion  string // fixed version if the scanner reports one, else ""
	Severity       string // CRITICAL/HIGH/MEDIUM/LOW or empty
	AdvisoryID     string // OSV-/GHSA-/CVE-/GO- ID
}

// fixedVersionFromRanges extracts the last "fixed" event from OSV-style
// affected[].ranges[].events[], returning "" when there is none. A fixed
// version is what makes a finding actionable, so extracting it correctly is
// the most important part of normalisation.
func fixedVersionFromRanges(ranges []osvRange) string {
	var fixed string
	for _, r := range ranges {
		for _, e := range r.Events {
			if e.Fixed != "" {
				fixed = e.Fixed // later ranges may supersede earlier ones
			}
		}
	}
	return fixed
}

type osvRange struct {
	Type   string    `json:"type"`
	Events []osvEvent `json:"events"`
}

type osvEvent struct {
	Introduced interface{} `json:"introduced"` // may be "0" or a version
	Fixed      string      `json:"fixed"`
}
