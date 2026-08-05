package scan

import "encoding/json"

// renovateReport models `renovate --report-type=json` (dry-run) output.
// Fields have drifted across Renovate versions, so both the *Version and *Value
// forms are read and the parser is tolerant of missing fields.
type renovateReport struct {
	Repositories []struct {
		Updates []struct {
			DepName        string `json:"depName"`
			CurrentValue   string `json:"currentValue"`
			NewValue       string `json:"newValue"`
			UpdateType     string `json:"updateType"`
			CurrentVersion string `json:"currentVersion"`
			NewVersion     string `json:"newVersion"`
			PackageFile    string `json:"packageFile"`
		} `json:"updates"`
	} `json:"repositories"`
}

// ParseRenovate turns a Renovate dry-run JSON report into the available-update
// set. Entries without a package name are skipped; the repo is not known here
// (scanjob stamps it later, mirroring findings).
func ParseRenovate(in []byte) ([]Update, error) {
	var rep renovateReport
	if err := json.Unmarshal(in, &rep); err != nil {
		return nil, err
	}
	var out []Update
	for _, repo := range rep.Repositories {
		for _, u := range repo.Updates {
			if u.DepName == "" {
				continue
			}
			nv := u.NewVersion
			if nv == "" {
				nv = u.NewValue
			}
			cv := u.CurrentVersion
			if cv == "" {
				cv = u.CurrentValue
			}
			out = append(out, Update{
				Ecosystem:      ecosystemFromPackageFile(u.PackageFile),
				Package:        u.DepName,
				CurrentVersion: cv,
				TargetVersion:  nv,
				UpdateType:     u.UpdateType,
			})
		}
	}
	return out, nil
}

// ecosystemFromPackageFile maps a manifest file path to a short ecosystem name,
// kept consistent with the scanner parsers. '' when unknown.
func ecosystemFromPackageFile(path string) string {
	switch {
	case path == "":
		return ""
	case path == "package.json" || hasSuffix(path, "/package.json"):
		return "npm"
	case path == "go.mod" || hasSuffix(path, "/go.mod"):
		return "Go"
	case hasSuffix(path, "requirements.txt") || path == "pyproject.toml" || path == "Pipfile":
		return "PyPI"
	case path == "Gemfile" || hasSuffix(path, "/Gemfile"):
		return "gem"
	case path == "pom.xml" || hasSuffix(path, "/pom.xml"):
		return "Maven"
	case hasSuffix(path, ".csproj"):
		return "NuGet"
	case path == "Cargo.toml" || hasSuffix(path, "/Cargo.toml"):
		return "crates.io"
	default:
		return ""
	}
}

func hasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}
