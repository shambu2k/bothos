package graphcache

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func baseCfg() ExtractionConfig {
	return ExtractionConfig{
		CorpusFilter: "code-only",
		Languages:    []string{"go", "ts"},
		Flags:        map[string]string{"prune_tests": "true"},
	}
}

func TestKeyDeterministicAndSha256(t *testing.T) {
	k := Key("v1.0", baseCfg(), "abc123")
	if k != Key("v1.0", baseCfg(), "abc123") {
		t.Fatal("key not deterministic")
	}
	if len(k) != sha256.Size*2 {
		t.Fatalf("key len %d, want %d", len(k), sha256.Size*2)
	}
	// sanity: it is hex of a sha256
	if _, err := hex.DecodeString(k); err != nil {
		t.Fatalf("key not hex: %v", err)
	}
}

func TestKeyChangesOnAnyComponent(t *testing.T) {
	base := Key("v1.0", baseCfg(), "abc123")
	changes := map[string]string{
		"tool version": Key("v1.1", baseCfg(), "abc123"),
		"config":       Key("v1.0", ExtractionConfig{CorpusFilter: "docs"}, "abc123"),
		"tree sha":     Key("v1.0", baseCfg(), "def456"),
	}
	for name, k := range changes {
		if k == base {
			t.Errorf("key did not change when %s changed", name)
		}
	}
}

func TestKeyCanonicalConfig(t *testing.T) {
	// Map insertion order must not matter: the same logical config keys to the
	// same graph.
	a := baseCfg()
	b := ExtractionConfig{
		CorpusFilter: "code-only",
		Languages:    []string{"go", "ts"},
		Flags:        map[string]string{"prune_tests": "true"},
	}
	if got, want := Key("v1.0", a, "abc"), Key("v1.0", b, "abc"); got != want {
		t.Fatalf("config order changed key: %q vs %q", got, want)
	}
}

func TestKeyIncludesCorpusFilter(t *testing.T) {
	// The code-only restriction is part of what the graph IS and must be in the
	// key (DESIGN.md). A docs-inclusive graph must not collide.
	codeOnly := Key("v1.0", ExtractionConfig{CorpusFilter: "code-only"}, "abc")
	inclusive := Key("v1.0", ExtractionConfig{CorpusFilter: "all", Languages: []string{"go"}}, "abc")
	if codeOnly == inclusive {
		t.Fatal("corpus filter not part of the key")
	}
}

func TestTreeSHAUsedNotCommit(t *testing.T) {
	// Two commits with the same tree share a graph; the doc is explicit that a
	// tree SHA (not commit SHA) keys the cache.
	same := Key("v", baseCfg(), "tree-abc")
	if got := Key("v", baseCfg(), "tree-abc"); got != same {
		t.Fatalf("same tree sha keyed identically, got %q want %q", got, same)
	}
	if Key("v", baseCfg(), "tree-abc") == Key("v", baseCfg(), "tree-def") {
		t.Fatal("different tree shas must key differently")
	}
}

func TestRetainKeepsNewestPerRepo(t *testing.T) {
	now := time.Now()
	entries := []Entry{
		{Key: "r1-old1", RepoID: "r1", BuiltAt: now.Add(-5 * time.Hour)},
		{Key: "r1-old2", RepoID: "r1", BuiltAt: now.Add(-4 * time.Hour)},
		{Key: "r1-new1", RepoID: "r1", BuiltAt: now.Add(-1 * time.Hour)},
		{Key: "r1-new2", RepoID: "r1", BuiltAt: now.Add(-30 * time.Minute)},
		{Key: "r1-newest", RepoID: "r1", BuiltAt: now},
	}
	keep, evict := Retain(entries, 3)
	if len(keep) != 3 {
		t.Fatalf("keep = %v, want 3", keep)
	}
	for _, want := range []string{"r1-newest", "r1-new2", "r1-new1"} {
		if !contains(keep, want) {
			t.Errorf("keep missing %q (kept %v)", want, keep)
		}
	}
	for _, want := range []string{"r1-old1", "r1-old2"} {
		if !contains(evict, want) {
			t.Errorf("evict missing %q (evicted %v)", want, evict)
		}
	}
}

func TestRetainInFlightSurvivesAge(t *testing.T) {
	now := time.Now()
	entries := []Entry{
		{Key: "newest", RepoID: "r1", BuiltAt: now},
		{Key: "old-inflight", RepoID: "r1", BuiltAt: now.Add(-48 * time.Hour), InFlight: true},
		{Key: "old2", RepoID: "r1", BuiltAt: now.Add(-40 * time.Hour)},
	}
	keep, _ := Retain(entries, 1)
	if !contains(keep, "old-inflight") {
		t.Errorf("in-flight graph evicted: %v", keep)
	}
	if !contains(keep, "newest") {
		t.Errorf("newest should be kept: %v", keep)
	}
}

func TestRetainPerRepoIndependent(t *testing.T) {
	now := time.Now()
	entries := []Entry{
		{Key: "a-new", RepoID: "a", BuiltAt: now},
		{Key: "a-old", RepoID: "a", BuiltAt: now.Add(-3 * time.Hour)},
		{Key: "b-new", RepoID: "b", BuiltAt: now.Add(-time.Hour)},
		{Key: "b-old", RepoID: "b", BuiltAt: now.Add(-2 * time.Hour)},
	}
	keep, evict := Retain(entries, 1)
	if contains(keep, "a-old") || contains(keep, "b-old") {
		t.Errorf("old graphs kept: %v", keep)
	}
	if !contains(keep, "a-new") || !contains(keep, "b-new") {
		t.Errorf("newest per repo not kept: %v", keep)
	}
	if len(evict) != 2 {
		t.Errorf("evict = %v, want 2", evict)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
