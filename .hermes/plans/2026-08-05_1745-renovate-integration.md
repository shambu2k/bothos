# Renovate Dry-Run Integration — finish Phase 1

> **For Hermes:** Use subagent-driven-development skill to implement this plan task-by-task.

**Goal:** Join a Renovate `--dry-run` into `bothos-scan` to turn the 21 fixed-version findings into *actionable upgrade candidates* (the input Phase 2 consumes).

**Architecture:** `bothos-scan` already clones → osv-scanner → upserts `findings`. We add one more step *in the same clone & run*: run Renovate in `--platform=local --dry-run` mode against the cloned tree, parse its JSON report into an `updates` table (the "available-update set"), then join `findings` ∩ `updates` to materialize actionable candidates. No LLM, no GitHub writes — still deterministic.

**Tech Stack:** Go, River/Postgres ledger, Renovate CLI (Node), osv-scanner.

---

## How it plugs in & works (explanation)

```
clone (temp dir)
   │
   ├─ osv-scanner --format json ──► ParseOSV ──► findings  (already done)
   │
   └─ renovate --platform=local --dry-run --report-type=json   (NEW)
           └─ renovate-report.json ──► ParseRenovate ──► updates  (NEW)
                                                        │
                        ledger: ActionableCandidates ────join────► candidates
                                     "finding has a fix AND an update is available"
```

1. **Renovate runs alongside osv-scanner** inside the same `bothos-scan` run, against the same shallow clone. `--platform=local` lets Renovate scan a local checkout with no remote/GitHub token; `--dry-run` means it only *computes* available updates, never opens PRs or mutates anything.
2. **JSON report** (`--report-type=json` → `renovate-report.json`) lists every dependency it resolves as an upgrade: `depName`, `currentValue/currentVersion`, `newValue/newVersion`, `updateType` (patch/minor/major). We parse this into an `updates` row per (repo, ecosystem, package).
3. **Ledger persists the "available-update set"** as an `updates` table (idempotent upsert), and a **join** `ActionableCandidates(repo)` returns every finding that (a) reports a fixed `target_version` **and** (b) has a matching available update — the concrete, PR-ready migration list for Phase 2.
4. **No new secrets.** `--platform=local` needs no GitHub token. Private registries are a config/risk item (open question below), not a default.

---

## Tasks

### Task 1: Add `updates` table to the ledger schema

**Objective:** A home for the Renovate available-update set.

**Files:**
- Modify: `internal/ledger/schema.sql`

Add (mirroring `findings` conventions; unique key lets periodic scans upsert in place):

```sql
CREATE TABLE IF NOT EXISTS updates (
    id              BIGSERIAL PRIMARY KEY,
    repo_id         TEXT NOT NULL,
    ecosystem       TEXT,
    package         TEXT NOT NULL,
    current_version TEXT,
    target_version  TEXT,
    update_type     TEXT,             -- patch | minor | major | pin | digest
    run_id          TEXT REFERENCES runs(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_updates_repo ON updates(repo_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_updates_dedup ON updates(repo_id, ecosystem, package);
```

**Verification:** migrate is idempotent (`go test -run TestMigrateIdempotent -p 1 ./internal/ledger/`).

---

### Task 2: `UpsertUpdates` (idempotent) + integration test

**Objective:** Persist the available-update set without duplication.

**Files:**
- Modify: `internal/ledger/ledger.go`
- Test: `internal/ledger/ledger_test.go`

Mirror `UpsertFindings`: `func (p *Postgres) UpsertUpdates(ctx, runID string, updates []UpdateRow) error` using a `pgx.Batch` + `ON CONFLICT (repo_id, ecosystem, package) DO UPDATE`. Define `UpdateRow` (repo, ecosystem, package, current, target, updateType).

**Test (`TestUpsertUpdatesIdempotent`):** seed a `runs` row, upsert 2 updates, assert 2 rows; upsert one with a new target, assert still 2 rows and the updated value. (Ignore `ecosystem` conflicts across ecosystems where a package appears in two ecosystems — note as a caveat.)

**TDD:** write test → RED → implement → GREEN.

---

### Task 3: `ActionableCandidates` join + integration test

**Objective:** Return findings with a fix that also have an available update.

**Files:**
- Modify: `internal/ledger/ledger.go`
- Test: `internal/ledger/ledger_test.go`

```go
// ActionableCandidates returns findings (repo, ecosystem, package, from, to,
// severity, advisory) where the finding has a target_version AND an available
// update exists for the same package. This is the Phase 2 PR-ready list.
func (p *Postgres) ActionableCandidates(ctx, repoID string) ([]Candidate, error)
```

Join SQL:
```sql
SELECT f.repo_id, f.package, f.current_version, f.target_version, f.severity, f.advisory_id, u.update_type
FROM findings f
JOIN updates u ON u.repo_id = f.repo_id AND u.package = f.package
WHERE f.repo_id = $1
  AND f.target_version IS NOT NULL AND f.target_version <> ''
ORDER BY (f.severity='CRITICAL') DESC, (f.severity='HIGH') DESC, f.package
```

**Test:** seed a finding with `target_version` + a matching `updates` row → candidate returned; finding without an available update → excluded.

**TDD:** write test → RED → implement → GREEN.

---

### Task 4: Renovate JSON report parser

**Objective:** Turn `renovate-report.json` into `[]UpdateRow`.

**Files:**
- Create: `internal/scan/renovate.go`
- Test: `internal/scan/renovate_test.go`

Define `Update` (Scanner, RepoID, Ecosystem, Package, CurrentVersion, TargetVersion, UpdateType) and:

```go
func ParseRenovate(in []byte) ([]Update, error)
```

Expected report shape (Renovate `--report-type=json`, dry-run):

```json
{ "repositories": [ {
    "repository": "local",
    "problems": [],
    "updates": [ {
        "packageFile": "package.json",
        "depName": "express",
        "currentValue": "^4.17.0",
        "newValue": "^4.19.0",
        "updateType": "minor",
        "currentVersion": "4.17.0",
        "newVersion": "4.19.0",
        "isMajor": false
    } ]
} ] }
```

Map `depName→Package`, `newVersion(or newValue)→TargetVersion`, `currentVersion→CurrentVersion`. Ecosystem inference from `packageFile` extension where easy; empty otherwise. **Fixture tests:** full report with mixed updateTypes, an empty `updates` array, and a malformed doc (error). TDD.

> **Note:** Pin the exact field names by running Renovate dry-run on a scratch repo in Task 6/7 — adjust parser to observed output if the schema differs (Renovate fields have drifted between versions). Keep `ParseRenovate` tolerant (skip entries missing `newVersion` and `depName`).

---

### Task 5: Renovate runner (exec `renovate` in the clone, read report file)

**Objective:** Run Renovate locally against the cloned dir and return the parsed updates.

**Files:**
- Create: `internal/scan/renovate_run.go` (or `run.go` addition)
- Test: `internal/scan/renovate_run_test.go`

```go
// RunRenovate runs `renovate --platform=local --dry-run --report-type=json`
// with cwd=dir, then parses the renovate-report.json it writes. Like osv-scanner,
// a non-zero exit is not necessarily a failure — dry-run may exit non-zero.
func RunRenovate(ctx context.Context, dir string, bin string) ([]Update, error)
```

Because Renovate writes a *file* rather than stdout (unlike the osv/trivy/govuln `Tool`), keep it a distinct function rather than forcing it into `Tool`. **Test with a fake `renovate` script** that writes a fixture `renovate-report.json` into cwd and exits 0/1 — verifying cwd handling and file read. TDD.

---

### Task 6: Wire Renovate into `scanjob` orchestration

**Objective:** `bothos-scan` runs Renovate after the scanners and persists updates + candidates.

**Files:**
- Modify: `internal/scanjob/scanjob.go`
- Test: `internal/scanjob/scanjob_test.go`

Extend `Config` with:
```go
type Config struct {
    Clone    func(ctx, dir, repo) error        // as today
    Tools    []scan.Tool                       // osv-scanner, as today
    Renovate func(ctx, dir string) ([]scan.Update, error) // injectable; real = scan.RunRenovate
}
```
Extend the upserter interface with `UpsertUpdates`. `Run` order:
1. clone
2. scanners → findings → `UpsertFindings`
3. Renovate → updates → `UpsertUpdates`
4. (candidates are *computed by the ledger join*, not stored — Phase 2 queries `ActionableCandidates`)

Return `(nFindings, nUpdates, error)`. **Update the existing scanjob test** with a fake Renovate returning one update and assert it's persisted. TDD.

---

### Task 7: `bothos-scan` CLI + container changes

**Objective:** Make the pipeline runnable end-to-end, Renovate included.

**Files:**
- Modify: `cmd/scan/main.go` — call the extended `scanjob.Run`, log findings + updates counts, report candidate count via `ledger.ActionableCandidates` for the repo.
- Modify: `Dockerfile` — add Node + Renovate to the runtime so `RunRenovate` has a binary. Two choices:
  - **A (recommended):** `FROM node:20-alpine AS renovate_build; RUN npm i -g renovate ...; COPY --from=renovate_build /usr/local/lib/node_modules ...` and copy the `renovate` launcher into the runtime image.
  - **B (sidecar):** a separate `renovate` compose service (official `renovate/renovate` image); scanjob shells to it via `docker run` against a shared clone volume. More moving parts; only if A is too heavy.
  - Default to **A**; note build-time cost (Renovate is a large npm package — first build is slow) as an accepted one-time cost.
- Modify: `deploy/docker-compose.yml` if B — otherwise no change.

**Verify:** `docker compose up -d --build worker`, then run the real scan:
```
docker exec deploy-worker-1 /usr/local/bin/bothos-scan -repo Jivanex/JIVA_BACKEND
```
Expected: log shows `N findings, M updates`, and `ActionableCandidates` returns a non-empty PR-ready list for JIVA. Confirm in DB:
```
SELECT * FROM updates WHERE repo_id='Jivanex/JIVA_BACKEND' ORDER BY update_type;
SELECT * FROM ActionableCandidates(repo)` (via ledger function / SQL join)
```

---

### Task 8: Documentation & commit

**Objective:** Record behavior + commit Phase 1 completion.

**Files:**
- Modify: `README.md` / `docs/` — one short note that Phase 1 now includes Renovate dry-run → candidates.
- `deploy/.env.example` — comment documenting any Renovate config knobs (none required by default).

**Commit** (conventional, one per task as you go; final umbrella optional):
```
feat(scan): join Renovate dry-run to turn findings into candidates
```

---

## Validation (overall)

- `make test` (`go test -p 1 ./...`) green — all new parser/ledger/scanjob tests.
- Live: `bothos-scan -repo Jivanex/JIVA_BACKEND` (private, via `GITHUB_READ_TOKEN`) → findings + updates + non-empty candidates.
- No GitHub writes, no new secrets; `renovate --platform=local --dry-run` never opens a PR or contacts the remote.

## Risks, trade-offs, open questions

- **Renovate image size / build time** — largest risk (Node + npm deps). Mitigation: option A with a one-time slow build, or option B sidecar if unacceptable.
- **Renovate report schema drift** — field names (`newVersion` vs `newValue`, `depName`) vary by version. Mitigation: Task 4 keeps the parser tolerant + Task 7 pins against real output.
- **Per-ecosystem fidelity** — Renovate extracts reliably for npm/Go/Python/Rust/NuGet/Maven/etc., but its coverage mirrors installed tooling; C++ stays weak (same honest caveat as osv-scanner). Where Renovate needs a package manager binary (e.g. some lockfile operations), it may require that runtime in the image — scope to what JIVA/BACKEND actually needs first.
- **Join semantics** — "candidate" = finding has a fix AND an update exists for the package. Precise var-boundary matching (update-to >= fix-to) is deferred; the default join is a conservative superset that Phase 2 filters. Note this explicitly so Phase 2 doesn't over-trust it.
- **Private registries** — if a repo uses private npm/pypi registries, Renovate needs host rules + auth; documented as a follow-up, not required for JIVA (public npm).
- **Open question:** is a persist-the-updates-table worth it, or should we only compute candidates on the fly? Plan keeps both (updates = auditability; candidates = join) — reconsider if storage bloat appears.
