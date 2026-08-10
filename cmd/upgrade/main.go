// Command upgrade turns Phase 1 actionable candidates for a repo into queued
// upgrade runs (one per candidate). It is the scheduler half of Phase 2; run it
// on a schedule in the awake window (or once to bootstrap). It writes nothing
// to GitHub itself — the worker + executor do that only after the LLM agent
// produces a tested bump. No credentials are required here.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"strings"

	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/queue"
	"github.com/shambu2k/bothos/internal/upgrade"
)

func main() {
	var (
		repo    = flag.String("repo", "", "owner/name to schedule upgrades for")
		baseRef = flag.String("base-ref", "", "base branch upgrades target (empty = resolve the repo's default)")
		enabled = flag.Bool("enabled", false, "explicit per-repo opt-in; refused when false")
		dsn     = flag.String("dsn", envOr("DATABASE_URL", ""), "Postgres DSN")
	)
	flag.Parse()

	if *repo == "" {
		log.Fatal("-repo is required (owner/name)")
	}
	if *dsn == "" {
		log.Fatal("DATABASE_URL (or -dsn) is required")
	}
	owner, name, ok := strings.Cut(*repo, "/")
	if !ok || owner == "" || name == "" {
		log.Fatalf("invalid repo %q (want owner/name)", *repo)
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
	q, err := queue.Open(ctx, *dsn, nil, nil)
	if err != nil {
		log.Fatalf("queue: %v", err)
	}
	defer q.Close()

	s := &upgrade.Scheduler{Ledger: l, Queue: q}
	n, err := s.Schedule(ctx, owner, name, *baseRef, *enabled)
	if err != nil {
		log.Fatalf("schedule %s: %v", *repo, err)
	}
	log.Printf("scheduled %d upgrade run(s) for %s", n, *repo)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
