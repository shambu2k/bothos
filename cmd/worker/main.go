// Command worker dequeues run jobs and processes them. Phase 0 stub: it marks
// the run running then succeeded and logs — delivery is the point; the LLM and
// sandbox arrive in Phase 2+. It holds no credentials.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/riverqueue/river"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/queue"
)

func main() {
	var (
		dsn       = flag.String("dsn", envOr("DATABASE_URL", "postgres://maintbot:maintbot-dev@localhost:5432/maintbot"), "Postgres DSN")
		queueName = flag.String("queue", "default", "river queue to consume")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	l, err := ledger.New(ctx, *dsn)
	if err != nil {
		log.Fatalf("ledger: %v", err)
	}
	defer l.Close()

	var handler queue.RunHandler = func(ctx context.Context, runID string) error {
		_ = l.SetRunStatus(ctx, runID, ledger.RunRunning)
		log.Printf("run %s: processing (Phase 0 stub — no LLM)", runID)
		if err := l.SetRunStatus(ctx, runID, ledger.RunSucceeded); err != nil {
			return err
		}
		return nil
	}

	q, err := queue.Open(ctx, *dsn, map[string]river.QueueConfig{
		*queueName: {MaxWorkers: 4},
	}, handler)
	if err != nil {
		log.Fatalf("queue: %v", err)
	}
	defer q.Close()

	if err := q.Client().Start(ctx); err != nil {
		log.Fatalf("start client: %v", err)
	}
	log.Printf("worker consuming queue %q", *queueName)

	<-ctx.Done()
	log.Println("worker shutting down")
	q.Client().Stop(context.Background())
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
