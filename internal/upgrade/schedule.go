package upgrade

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/queue"
)

// UpgradeMeta is the run.meta payload carried on a security run. It records the
// run's scope and the base branch the scheduler resolved at dispatch time; the
// executor targets the repo's origin/HEAD, which is the single source of truth.
type UpgradeMeta struct {
	Scope   string `json:"scope"` // "security"
	BaseRef string `json:"base_ref"`
}

// GrantForUpgrade builds the immutable grant for a scheduled security run: it
// may only open a PR, with a contents-write credential, capped and denied-path
// guarded, and it expires ~45 min out (one agent run). baseRef is recorded
// policy context only — the executor targets origin/HEAD, not this value.
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

// Scheduler enqueues one security run per repo on the schedule. The agent does
// its own discovery inside the sandbox, so unlike the old candidate loop there
// is exactly one run per repo (not one per finding) — the ledger's actionable
// candidate query is observability only and stays with cmd/scan.
type Scheduler struct {
	Ledger *ledger.Postgres
	Queue  *queue.Queue
}

// Schedule queues a single security run for repo when upgrades are enabled and
// one is not already outstanding, returning how many it queued (0 or 1).
// baseRefOverride sets the recorded base ref; empty resolves the repo's default
// branch. upgradesEnabled is the explicit per-repo opt-in; without it nothing
// is queued and no error is returned.
func (s *Scheduler) Schedule(ctx context.Context, owner, name, baseRefOverride string, upgradesEnabled bool) (int, error) {
	if !upgradesEnabled {
		return 0, nil
	}
	repoID := owner + "/" + name
	skip, err := s.alreadyScheduled(ctx, repoID)
	if err != nil {
		return 0, err
	}
	if skip {
		return 0, nil
	}

	base := baseRefOverride
	if base == "" {
		base, err = ResolveDefaultBranch(ctx, repoID)
		if err != nil {
			return 0, err
		}
	}

	meta, _ := json.Marshal(UpgradeMeta{Scope: "security", BaseRef: base})
	repo := intent.Repo{Owner: owner, Name: name, AccountID: owner}
	runID := newID()
	g, _ := json.Marshal(GrantForUpgrade(runID, repo, base))
	run := ledger.Run{ID: runID, RepoID: repoID, Trigger: "upgrade",
		ScopeKind: "scheduled", Grant: g, Decision: "allow", Status: ledger.RunQueued, Meta: meta}

	tx, err := s.Queue.Pool().Begin(ctx)
	if err != nil {
		return 0, err
	}
	if err := s.Ledger.InsertRunTx(ctx, tx, run); err != nil {
		_ = tx.Rollback(ctx)
		return 0, err
	}
	if err := s.Queue.EnqueueTx(ctx, tx, runID); err != nil {
		_ = tx.Rollback(ctx)
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return 1, nil
}

// alreadyScheduled reports whether the repo already has an outstanding
// (queued or running) upgrade run. Only non-terminal states suppress a new
// one — a succeeded run is done, not outstanding, and must not block the next
// schedule (a regression bit us: 3 succeeded runs blocked re-scheduling
// forever).
func (s *Scheduler) alreadyScheduled(ctx context.Context, repo string) (bool, error) {
	var c int
	err := s.Queue.Pool().QueryRow(ctx, `
		SELECT count(*) FROM runs
		WHERE repo_id=$1 AND trigger='upgrade'
		  AND status IN ('queued','running')`, repo).Scan(&c)
	return c > 0, err
}

// ResolveDefaultBranch returns the repo's default branch short name (e.g.
// "main") via `git ls-remote --symref <url> HEAD`. The URL is built exactly as
// in scanjob.ShallowClone so private-repo auth matches the clone path.
func ResolveDefaultBranch(ctx context.Context, repo string) (string, error) {
	url := "https://github.com/" + repo + ".git"
	if tok := os.Getenv("GITHUB_READ_TOKEN"); tok != "" {
		url = "https://x-access-token:" + tok + "@github.com/" + repo + ".git"
	}
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--symref", url, "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ls-remote %s: %w: %s", repo, err, out)
	}
	// --symref emits a leading "ref: refs/heads/NAME\tHEAD" line.
	fields := strings.Fields(string(out))
	if len(fields) >= 2 && fields[0] == "ref:" && strings.HasPrefix(fields[1], "refs/heads/") {
		return strings.TrimPrefix(fields[1], "refs/heads/"), nil
	}
	return "", fmt.Errorf("unparseable ls-remote output for %s: %q", repo, strings.TrimSpace(string(out)))
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
