// Package ledger is the Postgres audit spine: runs, intents (with executor
// idempotency dedup), findings, graph cache metadata, capability gaps, and
// credential/config tables.
package ledger

import (
	"context"
	_ "embed"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shambu2k/bothos/internal/scan"
)

//go:embed schema.sql
var schemaSQL string

type Postgres struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Postgres{pool: pool}, nil
}

func (p *Postgres) Close() { p.pool.Close() }

// Migrate applies the (idempotent) schema.
func (p *Postgres) Migrate(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, schemaSQL); err != nil {
		return err
	}
	return nil
}

// ---------- executor.Ledger (intent idempotency) ----------

// Lookup reports whether an idempotency key was already executed, returning
// the GitHub ref it produced. Used by the executor to dedupe River retries and
// GitHub redeliveries.
func (p *Postgres) Lookup(ctx context.Context, idemKey string) (string, bool, error) {
	var ref string
	err := p.pool.QueryRow(ctx,
		`SELECT github_ref FROM intents WHERE idempotency_key = $1`, idemKey).Scan(&ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return ref, true, nil
}

// Record persists a successful intent execution keyed by its idempotency key.
func (p *Postgres) Record(ctx context.Context, idemKey, runID, githubRef string) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO intents(idempotency_key, run_id, github_ref, executed_at)
		 VALUES ($1, $2, $3, now())
		 ON CONFLICT (idempotency_key) DO NOTHING`,
		idemKey, runID, githubRef)
	return err
}

// ---------- runs ----------

type RunStatus string

const (
	RunQueued    RunStatus = "queued"
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunDenied    RunStatus = "denied"
)

type Run struct {
	ID          string
	RepoID      string
	Trigger     string // webhook_pull_request | webhook_issue_labeled | scheduled
	ScopeKind   string
	ScopeNumber int
	Grant       []byte
	Decision    string // allow | deny
	DenyReason  string
	Status      RunStatus
}

// InsertRun records a run at dispatch time. Every webhook lands here with its
// policy decision before anything async happens.
func (p *Postgres) InsertRun(ctx context.Context, r Run) error {
	return p.insertRun(ctx, p.pool, r)
}

// InsertRunTx inserts the run inside a caller-owned transaction so the run row
// and its River job commit atomically (the plan's River.InsertTx).
func (p *Postgres) InsertRunTx(ctx context.Context, tx pgx.Tx, r Run) error {
	return p.insertRun(ctx, tx, r)
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (p *Postgres) insertRun(ctx context.Context, ex execer, r Run) error {
	_, err := ex.Exec(ctx, `
		INSERT INTO runs(id, repo_id, trigger, scope_kind, scope_number, "grant", decision, deny_reason, status)
		VALUES ($1,$2,$3,$4,NULLIF($5,0),$6,$7,NULLIF($8,''),$9)`,
		r.ID, r.RepoID, r.Trigger, r.ScopeKind, r.ScopeNumber, r.Grant, r.Decision, r.DenyReason, r.Status)
	return err
}

// SetRunStatus transitions a run; a terminal call also stamps ended_at.
func (p *Postgres) SetRunStatus(ctx context.Context, id string, status RunStatus) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE runs SET status=$2, ended_at=CASE WHEN $2 IN ('succeeded','failed','denied') THEN now() ELSE ended_at END
		WHERE id=$1`, id, status)
	return err
}

// ---------- capability gap ----------

func (p *Postgres) RecordCapabilityGap(ctx context.Context, runID, requestedKind, context_ string) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO capability_gaps(run_id, requested_kind, context) VALUES ($1,$2,NULLIF($3,''))`,
		runID, requestedKind, context_)
	return err
}

// ---------- findings ----------

// UpsertFindings inserts or refreshes findings in place, keyed by
// (repo_id, scanner, package, advisory_id), so repeated scans never duplicate
// a row. runID links the scan that produced them.
func (p *Postgres) UpsertFindings(ctx context.Context, runID string, findings []scan.Finding) error {
	if len(findings) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, f := range findings {
		batch.Queue(`
			INSERT INTO findings(repo_id, scanner, ecosystem, package, current_version,
			                     target_version, severity, advisory_id, status, run_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'open',$9)
			ON CONFLICT (repo_id, scanner, package, advisory_id)
			DO UPDATE SET ecosystem=EXCLUDED.ecosystem,
			              current_version=EXCLUDED.current_version,
			              target_version=EXCLUDED.target_version,
			              severity=EXCLUDED.severity,
			              status='open',
			              run_id=EXCLUDED.run_id`,
			f.RepoID, string(f.Scanner), f.Ecosystem, f.Package,
			f.CurrentVersion, f.TargetVersion, f.Severity, f.AdvisoryID, runID)
	}
	br := p.pool.SendBatch(ctx, batch)
	defer br.Close()
	for range findings {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return br.Close()
}

// UpsertUpdates inserts or refreshes the Renovate available-update set in
// place, keyed on (repo_id, ecosystem, package), so repeated dry-runs never
// duplicate a row. runID links the scan that produced it.
func (p *Postgres) UpsertUpdates(ctx context.Context, runID string, ups []scan.Update) error {
	if len(ups) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, u := range ups {
		batch.Queue(`
			INSERT INTO updates(repo_id, ecosystem, package, current_version,
			                   target_version, update_type, run_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (repo_id, ecosystem, package)
			DO UPDATE SET current_version=EXCLUDED.current_version,
			              target_version=EXCLUDED.target_version,
			              update_type=EXCLUDED.update_type,
			              run_id=EXCLUDED.run_id`,
			u.RepoID, u.Ecosystem, u.Package, u.CurrentVersion,
			u.TargetVersion, u.UpdateType, runID)
	}
	br := p.pool.SendBatch(ctx, batch)
	defer br.Close()
	for range ups {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return br.Close()
}

// Findings returns a repo's current findings for manual-audit comparison
// against a scan run.
func (p *Postgres) Findings(ctx context.Context, repoID string) ([]scan.Finding, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT repo_id, scanner, ecosystem, package, current_version,
		       target_version, severity, advisory_id
		FROM findings WHERE repo_id=$1 ORDER BY severity DESC NULLS LAST, package`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scan.Finding
	for rows.Next() {
		var f scan.Finding
		var sc string
		if err := rows.Scan(&f.RepoID, &sc, &f.Ecosystem, &f.Package,
			&f.CurrentVersion, &f.TargetVersion, &f.Severity, &f.AdvisoryID); err != nil {
			return nil, err
		}
		f.Scanner = scan.Scanner(sc)
		out = append(out, f)
	}
	return out, rows.Err()
}
