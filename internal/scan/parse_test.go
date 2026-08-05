package scan

import (
	"reflect"
	"testing"
)

func TestParseOSV(t *testing.T) {
	fixture := `{
	  "results": [
	    {
	      "source": {"path": "go.mod", "type": "lockfile"},
	      "packages": [
	        {
	          "package": {"name": "github.com/golang-jwt/jwt/v4", "version": "v4.4.2", "ecosystem": "Go"},
	          "vulnerabilities": [
	            {
	              "id": "GHSA-m6cx-g6qm-p2c8",
	              "database_specific": {"severity": "HIGH"},
	              "affected": [
	                {"ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}, {"fixed": "v4.5.0"}]}]}
	              ]
	            }
	          ]
	        }
	      ]
	    }
	  ]
	}`
	got, err := ParseOSV([]byte(fixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []Finding{{
		Scanner: ScannerOSV, Ecosystem: "Go",
		Package: "github.com/golang-jwt/jwt/v4", CurrentVersion: "v4.4.2",
		TargetVersion: "v4.5.0", Severity: "HIGH", AdvisoryID: "GHSA-m6cx-g6qm-p2c8",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestParseOSVNoFixedVersion(t *testing.T) {
	fixture := `{
	  "results": [{
	    "packages": [{
	      "package": {"name": "leftpad", "version": "0.0.1", "ecosystem": "npm"},
	      "vulnerabilities": [{"id": "GHSA-xxxx", "affected": [{"ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}]}]}]}]
	    }]
	  }]
	}`
	got, err := ParseOSV([]byte(fixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].TargetVersion != "" || got[0].Severity != "" {
		t.Fatalf("expected one finding with no fixed version and no severity, got %+v", got)
	}
}

func TestParseTrivy(t *testing.T) {
	fixture := `{
	  "Results": [{
	    "Target": "app/Gemfile.lock",
	    "Class": "lang-pkgs",
	    "Vulnerabilities": [{
	      "VulnerabilityID": "CVE-2024-0101",
	      "PkgName": "nokogiri",
	      "InstalledVersion": "1.15.4",
	      "FixedVersion": "1.16.0",
	      "Severity": "HIGH",
	      "PkgIdentifier": {"PURL": "pkg:gem/nokogiri@1.15.4"}
	    }]
	  }]
	}`
	got, err := ParseTrivy([]byte(fixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []Finding{{
		Scanner: ScannerTrivy, Ecosystem: "gem",
		Package: "nokogiri", CurrentVersion: "1.15.4",
		TargetVersion: "1.16.0", Severity: "HIGH", AdvisoryID: "CVE-2024-0101",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestParseGovuln(t *testing.T) {
	fixture := `{
	  "version": "v1.0.0",
	  "vulns": [{
	    "osv": {"id": "GO-2024-1234", "details": "x", "affected": [{"package": {"name": "golang.org/x/net", "ecosystem": "Go"}}]},
	    "modules": [{"path": "golang.org/x/net", "version": "v0.20.0", "fixed_version": "v0.32.0"}]
	  }]
	}`
	got, err := ParseGovuln([]byte(fixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []Finding{{
		Scanner: ScannerGovuln, Ecosystem: "Go",
		Package: "golang.org/x/net", CurrentVersion: "v0.20.0",
		TargetVersion: "v0.32.0", AdvisoryID: "GO-2024-1234",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}
