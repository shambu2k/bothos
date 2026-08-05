package scan

import (
	"reflect"
	"testing"
)

func TestParseRenovate(t *testing.T) {
	fixture := `{
	  "repositories": [{
	    "repository": "local",
	    "updates": [
	      {"packageFile": "package.json", "depName": "express", "currentValue": "^4.17.0", "newValue": "^4.19.0", "updateType": "minor", "currentVersion": "4.17.0", "newVersion": "4.19.0"},
	      {"packageFile": "package.json", "depName": "tar", "currentValue": "7.5.16", "newValue": "7.5.19", "updateType": "patch", "currentVersion": "7.5.16", "newVersion": "7.5.19"}
	    ]
	  }]
	}`
	got, err := ParseRenovate([]byte(fixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []Update{
		{Ecosystem: "npm", Package: "express", CurrentVersion: "4.17.0", TargetVersion: "4.19.0", UpdateType: "minor"},
		{Ecosystem: "npm", Package: "tar", CurrentVersion: "7.5.16", TargetVersion: "7.5.19", UpdateType: "patch"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestParseRenovateFallsBackToValueFields(t *testing.T) {
	// Older Renovate reports only currentValue/newValue (no Version fields).
	fixture := `{"repositories":[{"updates":[
	  {"depName": "leftpad", "currentValue": "0.0.1", "newValue": "0.0.2", "updateType": "patch", "packageFile": "package.json"}
	]}]}`
	got, err := ParseRenovate([]byte(fixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].Package != "leftpad" || got[0].CurrentVersion != "0.0.1" || got[0].TargetVersion != "0.0.2" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestParseRenovateEmpty(t *testing.T) {
	got, err := ParseRenovate([]byte(`{"repositories":[{"updates":[]}]}`))
	if err != nil {
		t.Fatalf("parse empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 updates, got %d", len(got))
	}
}

func TestParseRenovateMalformed(t *testing.T) {
	if _, err := ParseRenovate([]byte(`not json`)); err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}
