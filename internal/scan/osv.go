package scan

import "encoding/json"

// osvRoot is the shape of `osv-scanner --format json`.
type osvRoot struct {
	Results []struct {
		Packages []osvPackage `json:"packages"`
	} `json:"results"`
}

type osvPackage struct {
	Package struct {
		Name      string `json:"name"`
		Version   string `json:"version"`
		Ecosystem string `json:"ecosystem"`
	} `json:"package"`
	Vulnerabilities []osvVuln `json:"vulnerabilities"`
}

type osvVuln struct {
	ID               string `json:"id"`
	DatabaseSpecific *struct {
		Severity string `json:"severity"`
	} `json:"database_specific"`
	Affected []struct {
		Ranges []osvRange `json:"ranges"`
	} `json:"affected"`
}

// ParseOSV normalises `osv-scanner --format json` output into findings.
func ParseOSV(in []byte) ([]Finding, error) {
	var root osvRoot
	if err := json.Unmarshal(in, &root); err != nil {
		return nil, err
	}
	var out []Finding
	for _, res := range root.Results {
		for _, p := range res.Packages {
			for _, v := range p.Vulnerabilities {
				var ranges []osvRange
				for _, a := range v.Affected {
					ranges = append(ranges, a.Ranges...)
				}
				sev := ""
				if v.DatabaseSpecific != nil {
					sev = v.DatabaseSpecific.Severity
				}
				out = append(out, Finding{
					Scanner:        ScannerOSV,
					Ecosystem:      p.Package.Ecosystem,
					Package:        p.Package.Name,
					CurrentVersion: p.Package.Version,
					TargetVersion:  fixedVersionFromRanges(ranges),
					Severity:       sev,
					AdvisoryID:     v.ID,
				})
			}
		}
	}
	return out, nil
}
