package scan

import "encoding/json"

// trivyRoot is the shape of `trivy fs --format json`.
type trivyRoot struct {
	Results []struct {
		Vulnerabilities []trivyVuln `json:"Vulnerabilities"`
	} `json:"Results"`
}

type trivyVuln struct {
	VulnerabilityID  string `json:"VulnerabilityID"`
	PkgName          string `json:"PkgName"`
	InstalledVersion string `json:"InstalledVersion"`
	FixedVersion     string `json:"FixedVersion"`
	Severity         string `json:"Severity"`
	PkgIdentifier    struct {
		PURL string `json:"PURL"`
	} `json:"PkgIdentifier"`
}

// ParseTrivy normalises `trivy ... --format json` output into findings.
func ParseTrivy(in []byte) ([]Finding, error) {
	var root trivyRoot
	if err := json.Unmarshal(in, &root); err != nil {
		return nil, err
	}
	var out []Finding
	for _, res := range root.Results {
		for _, v := range res.Vulnerabilities {
			out = append(out, Finding{
				Scanner:        ScannerTrivy,
				Ecosystem:      ecosystemFromPURL(v.PkgIdentifier.PURL),
				Package:        v.PkgName,
				CurrentVersion: v.InstalledVersion,
				TargetVersion:  v.FixedVersion,
				Severity:       v.Severity,
				AdvisoryID:     v.VulnerabilityID,
			})
		}
	}
	return out, nil
}

// ecosystemFromPURL maps a package URL type to a short ecosystem name, kept
// close to what osv-scanner reports so findings can be joined across scanners.
func ecosystemFromPURL(purl string) string {
	// purl form: pkg:<type>/<namespace>/<name>@<version>
	rest := purl
	for _, prefix := range []string{"pkg:", "pkg/"} {
		if len(rest) >= len(prefix) && rest[:len(prefix)] == prefix {
			rest = rest[len(prefix):]
			break
		}
	}
	typ := rest
	if i := indexByte(rest, '/'); i >= 0 {
		typ = rest[:i]
	}
	switch typ {
	case "npm":
		return "npm"
	case "golang":
		return "Go"
	case "pypi":
		return "PyPI"
	case "gem":
		return "gem"
	case "maven":
		return "Maven"
	case "nuget":
		return "NuGet"
	case "cargo":
		return "crates.io"
	default:
		return ""
	}
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
