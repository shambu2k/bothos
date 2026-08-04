package queue

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/shambu2k/maintainer-bot/internal/testdb"
)

func TestQueueEnqueuesAndWorksRunJob(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.DSN(t)
	testdb.Truncate(t, dsn, "river_job") // clear jobs left by other packages sharing the DB

	var mu sync.Mutex
	handled := []string{}
	done := make(chan struct{}, 1)

	q, err := Open(ctx, dsn, map[string]river.QueueConfig{
		"default": {MaxWorkers: 1},
	}, func(ctx context.Context, runID string) error {
		mu.Lock()
		handled = append(handled, runID)
		mu.Unlock()
		select {
		case done <- struct{}{}:
		default:
		}
		return nil
	})
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	defer q.Close()

	// Enqueue atomically with a transaction.
	tx, err := q.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := q.EnqueueTx(ctx, tx, "run-1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Run the worker loop until the job is processed.
	startCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := q.Client().Start(startCtx); err != nil {
		t.Fatalf("start client: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for job to be worked")
	}
	cancel()
	if err := q.Client().Stop(ctx); err != nil {
		t.Fatalf("stop client: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(handled) != 1 || handled[0] != "run-1" {
		t.Fatalf("handled = %v, want [run-1]", handled)
	}
}

func TestEnqueueTxRollsBack(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.DSN(t)
	testdb.Truncate(t, dsn, "river_job")

	q, err := Open(ctx, dsn, nil, func(ctx context.Context, runID string) error { return nil })
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	defer q.Close()

	// Roll back the tx, so the job must not persist.
	tx, err := q.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := q.EnqueueTx(ctx, tx, "run-lost"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var count int
	if err := q.Pool().QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE args->>'run_id' = $1`, "run-lost").Scan(&count); err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 0 {
		t.Fatalf("rolled-back job persisted: %d rows", count)
	}
}
