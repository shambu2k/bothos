package scan

import "encoding/json"

// govulnRoot is the shape of `govulncheck -format json`.
type govulnRoot struct {
	Vulns []struct {
		Osv struct {
			ID string `json:"id"`
		} `json:"osv"`
		Modules []struct {
			Path         string `json:"path"`
			Version      string `json:"version"`      // found/installed version
			FixedVersion string `json:"fixed_version"` // where available
		} `json:"modules"`
	} `json:"vulns"`
}

// ParseGovuln normalises `govulncheck -format json` output into findings.
func ParseGovuln(in []byte) ([]Finding, error) {
	var root govulnRoot
	if err := json.Unmarshal(in, &root); err != nil {
		return nil, err
	}
	var out []Finding
	for _, v := range root.Vulns {
		for _, m := range v.Modules {
			out = append(out, Finding{
				Scanner:        ScannerGovuln,
				Ecosystem:      "Go",
				Package:        m.Path,
				CurrentVersion: m.Version,
				TargetVersion:  m.FixedVersion,
				AdvisoryID:     v.Osv.ID,
			})
		}
	}
	return out, nil
}
