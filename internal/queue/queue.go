// Package queue wraps River (Postgres-backed transactional job queue) for the
// bot's deferred work. The gateway enqueues a run job inside the same
// transaction that inserts the run row; the worker dequeues and processes it.
package queue

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// RunJob is the unit of work for a deferred run. It carries only the run id;
// everything else lives in the ledger.
type RunJob struct {
	RunID string `json:"run_id"`
}

func (RunJob) Kind() string { return "run" }

// RunHandler processes a dequeued run. Phase 0's stub just logs (delivery is
// the point); later phases orchestrate sandbox -> runtime -> executor.
type RunHandler func(ctx context.Context, runID string) error

// Queue owns the pgx pool and the River client. The pool is shared so the
// gateway can, in one tx, insert the run row (ledger) and enqueue its job.
type Queue struct {
	pool   *pgxpool.Pool
	client *river.Client[pgx.Tx]
}

// Open connects, applies River's migrations, and registers the RunJob worker.
// queueCfg may be nil for a process that only enqueues.
func Open(ctx context.Context, dsn string, queueCfg map[string]river.QueueConfig, handler RunHandler) (*Queue, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := migrateRiver(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}

	workers := river.NewWorkers()
	if handler != nil {
		river.AddWorker(workers, river.WorkFunc(func(ctx context.Context, job *river.Job[RunJob]) error {
			return handler(ctx, job.Args.RunID)
		}))
	}

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:  queueCfg,
		Workers: workers,
	})
	if err != nil {
		pool.Close()
		return nil, err
	}
	return &Queue{pool: pool, client: client}, nil
}

func migrateRiver(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return err
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return err
	}
	return nil
}

func (q *Queue) Pool() *pgxpool.Pool                 { return q.pool }
func (q *Queue) Client() *river.Client[pgx.Tx]       { return q.client }

// EnqueueTx inserts the run job inside the caller's transaction so the run row
// and its job commit atomically — a redelivered or retried webhook cannot leave
// an orphaned job.
func (q *Queue) EnqueueTx(ctx context.Context, tx pgx.Tx, runID string) error {
	_, err := q.client.InsertTx(ctx, tx, &RunJob{RunID: runID}, nil)
	return err
}

// Close stops the worker loop and closes the pool.
func (q *Queue) Close() {
	if q.client != nil {
		_ = q.client.Stop(context.Background())
	}
	if q.pool != nil {
		q.pool.Close()
	}
}
