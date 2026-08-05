package upgrade

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/queue"
)

// UpgradeMeta is the run.meta payload carried on an upgrade run. It pairs a
// Phase 1 candidate with its upgrade target so the worker needs no other store.
type UpgradeMeta struct {
	Package    string `json:"pkg"`
	From       string `json:"from"`
	To         string `json:"to"`
	AdvisoryID string `json:"advisory_id"`
}

// GrantForUpgrade builds the immutable grant for a scheduled upgrade run: it
// may only open a PR, with a contents-write credential, capped and denied-path
// guarded, and it expires ~45 min out (one agent run).
func GrantForUpgrade(runID string, repo intent.Repo, baseRef string) intent.Grant {
	now := time.Now()
	return intent.Grant{
		RunID:        runID,
		Repo:         repo,
		Scope:        intent.Scope{Kind: intent.ScopeScheduled, BaseRef: baseRef},
		AllowedKinds: []intent.Kind{"open_pr"},
		TokenScope:   intent.TokenContentsWrite,
		Limits: intent.Limits{
			MaxIntents:     1,
			MaxFiles:       50,
			MaxDiffLines:   2000,
			MaxBodyBytes:   8000,
			MaxComments:    0,
			MaxLabelsAdded: 0,
		},
		DeniedPaths: []string{".env", ".env.*", "deploy/.env", "**/secrets/**", "**/*.pem", "**/*.key"},
		IssuedAt:    now,
		ExpiresAt:   now.Add(45 * time.Minute),
	}
}

// Scheduler enqueues one upgrade run per actionable candidate for a repo,
// skipping packages that already have a non-failed upgrade run. Per candidate
// it is atomic (run row + River job in one tx).
type Scheduler struct {
	Ledger *ledger.Postgres
	Queue  *queue.Queue
}

// Schedule creates queued upgrade runs for each candidate and returns how many.
// upgradesEnabled is the explicit per-repo opt-in; without it nothing is queued.
func (s *Scheduler) Schedule(ctx context.Context, owner, name, baseRef string, upgradesEnabled bool) (int, error) {
	if !upgradesEnabled {
		return 0, fmt.Errorf("upgrades not enabled for %s/%s", owner, name)
	}
	cands, err := s.Ledger.ActionableCandidates(ctx, owner+"/"+name)
	if err != nil {
		return 0, err
	}
	var n int
	for _, c := range cands {
		skip, err := s.alreadyScheduled(ctx, owner+"/"+name, c.Package)
		if err != nil {
			return n, err
		}
		if skip {
			continue
		}
		meta, _ := json.Marshal(UpgradeMeta{Package: c.Package, From: c.CurrentVersion, To: c.TargetVersion, AdvisoryID: c.AdvisoryID})
		repo := intent.Repo{Owner: owner, Name: name, AccountID: owner}
		runID := newID()
		g, _ := json.Marshal(GrantForUpgrade(runID, repo, baseRef))
		run := ledger.Run{ID: runID, RepoID: owner + "/" + name, Trigger: "upgrade",
			ScopeKind: "scheduled", Grant: g, Decision: "allow", Status: ledger.RunQueued, Meta: meta}

		tx, err := s.Queue.Pool().Begin(ctx)
		if err != nil {
			return n, err
		}
		if err := s.Ledger.InsertRunTx(ctx, tx, run); err != nil {
			_ = tx.Rollback(ctx)
			return n, err
		}
		if err := s.Queue.EnqueueTx(ctx, tx, runID); err != nil {
			_ = tx.Rollback(ctx)
			return n, err
		}
		if err := tx.Commit(ctx); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *Scheduler) alreadyScheduled(ctx context.Context, repo, pkg string) (bool, error) {
	var c int
	err := s.Queue.Pool().QueryRow(ctx, `
		SELECT count(*) FROM runs
		WHERE repo_id=$1 AND trigger='upgrade' AND meta->>'pkg'=$2
		  AND status NOT IN ('failed','denied')`, repo, pkg).Scan(&c)
	return c > 0, err
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
