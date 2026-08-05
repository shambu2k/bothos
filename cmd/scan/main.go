// Command scan runs a deterministic security scan of one repo and upserts the
// findings into the ledger, no LLM involved. It is the periodic Phase 1 scan
// job, runnable by cron or on demand. Holds no GitHub token.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"log"
	"os"

	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/scanjob"
)

func main() {
	var (
		repo = flag.String("repo", "", "owner/name to scan (required)")
		dsn  = flag.String("dsn", envOr("DATABASE_URL", "postgres://maintbot:maintbot-dev@localhost:5432/maintbot"), "Postgres DSN")
	)
	flag.Parse()
	if *repo == "" {
		log.Fatal("-repo owner/name is required")
	}

	ctx := context.Background()
	l, err := ledger.New(ctx, *dsn)
	if err != nil {
		log.Fatalf("ledger: %v", err)
	}
	defer l.Close()
	if err := l.Migrate(ctx); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	runID := "scan-" + newID()
	// Findings FK to runs, so record the scan run up front for the audit trail.
	if err := l.InsertRun(ctx, ledger.Run{
		ID: runID, RepoID: *repo, Trigger: "scheduled", ScopeKind: "scheduled",
		Grant: []byte(`{}`), Decision: "allow", Status: ledger.RunRunning,
	}); err != nil {
		log.Fatalf("insert run: %v", err)
	}

	n, err := scanjob.Run(ctx, scanjob.Config{}, l, *repo, runID)
	if err != nil {
		_ = l.SetRunStatus(ctx, runID, ledger.RunFailed)
		log.Fatalf("scan %s: %v", *repo, err)
	}
	if err := l.SetRunStatus(ctx, runID, ledger.RunSucceeded); err != nil {
		log.Fatalf("status: %v", err)
	}
	log.Printf("scan %s: %d findings (run %s)", *repo, n, runID)
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
