// Package graphcache keys and retains prebuilt codebase graphs so a run stays
// hermetic and reproducible: a graph is a derived artifact, a deterministic
// function of the tree, never memory.
//
// Key shape (DESIGN.md):
//
//	key = sha256(
//	    graphify_version   ||   // tool upgrade invalidates cleanly
//	    extraction_config  ||   // corpus filter, language set, flags
//	    tree_sha                // git rev-parse HEAD^{tree}
//	)
package graphcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

// ExtractionConfig describes what a graph was built from. It must be part of
// the key: the code-only restriction is part of what the graph *is*, so a
// graph built over a different corpus must never be served as this one.
type ExtractionConfig struct {
	CorpusFilter string            `json:"corpus_filter"` // e.g. "code-only"
	Languages    []string          `json:"languages,omitempty"`
	Flags        map[string]string `json:"flags,omitempty"`
}

// Key derives the deterministic cache key for a tree. build is
// canonical: struct field order is fixed and encoding/json sorts map keys, so
// the same logical config always keys to the same graph.
func Key(graphifyVersion string, cfg ExtractionConfig, treeSHA string) string {
	h := sha256.New()
	h.Write([]byte(graphifyVersion))
	h.Write([]byte{0xff})
	canon, _ := json.Marshal(cfg)
	h.Write(canon)
	h.Write([]byte{0xff})
	h.Write([]byte(treeSHA))
	return hex.EncodeToString(h.Sum(nil))
}

// Entry is one row of graph_cache(repo_id, tree_sha, key, built_at, bytes).
// InFlight marks graphs still referenced by an unfinished run — those survive
// retention regardless of age, because evicting them would break reproducibility
// of a run in progress.
type Entry struct {
	Key      string
	RepoID   string
	BuiltAt  time.Time
	InFlight bool
}

// Retain decides which graph blobs to keep: the N most recent per repo plus
// anything an unfinished run still references. Everything else is rebuildable
// by definition, so it is evictable.
func Retain(entries []Entry, keepPerRepo int) (keep []string, evict []string) {
	byRepo := map[string][]Entry{}
	for _, e := range entries {
		byRepo[e.RepoID] = append(byRepo[e.RepoID], e)
	}
	inFlight := map[string]bool{}
	for _, e := range entries {
		if e.InFlight {
			if !containsStr(keep, e.Key) {
				keep = append(keep, e.Key)
			}
			inFlight[e.Key] = true
		}
	}
	for _, list := range byRepo {
		sort.SliceStable(list, func(i, j int) bool {
			return list[i].BuiltAt.After(list[j].BuiltAt)
		})
		kept := 0
		for _, e := range list {
			if kept >= keepPerRepo && !inFlight[e.Key] {
				evict = append(evict, e.Key)
				continue
			}
			if !containsStr(keep, e.Key) {
				keep = append(keep, e.Key)
			}
			kept++
		}
	}
	return keep, evict
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
