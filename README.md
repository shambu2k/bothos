# bothos

Self-hosted, homelab-deployed, per-repo AI maintainer for GitHub.

Design and build plan live in [`docs/`](docs/): `MAINTAINER_BOT_PLAN.md`
(architecture, workloads, security model, phases) and `DESIGN.md` (intent
boundary and graph cache).

## Security model in one line

Issue bodies, PR descriptions, changelogs and advisories are attacker-controlled
and land in the agent's context. Assume the agent is hijacked on some runs, and
design so a hijacked agent's blast radius is *"wrote unhelpful words in the
right place."*

The core of that is the **intent boundary**: an agent runtime emits content-only
intents (never targeting), and a privileged executor supplies every *where does
this land* field from an immutable dispatch-time Grant. See
[`docs/DESIGN.md`](docs/DESIGN.md).

## Packages

| Package | Responsibility |
|---|---|
| `internal/intent` | Content-only intent schema + validation kernel (safe by construction) |
| `internal/executor` | Sole holder of PATs; resolves intents against GitHub (logic + go-github adapter) |
| `internal/policy` | Dispatch-time grants as data — the immutable capability surface per run |
| `internal/runtime` | `AgentRuntime` seam + named runtime registry + structured upgrade prompt |
| `internal/graphcache` | Deterministic graph cache keying + retention (`sha256(tool‖cfg‖tree)`) |
| `internal/ledger` | `runs`/`findings`/`updates` Postgres spine + actionable candidates |
| `internal/queue` | River-backed transactional queue + periodic jobs |
| `internal/scan` / `internal/scanjob` | Deterministic scanners (osv-scanner) → findings |
| `internal/agent` | PI agent via documented `--mode rpc` (persistent per-run sessions) + lifecycle hardening |
| `internal/upgrade` | Candidate→task, grant, scheduler, git diff source |
| `internal/runpipe` | Worker orchestration: sandbox → runtime → executor |
| `internal/credstore` | Executor-only write-PAT resolution |
| `cmd/gateway` | Webhook receiver (sig validation, ack-fast, dispatch) |
| `cmd/worker` | Run orchestration: sandbox → PI runtime → executor |
| `cmd/scan` | CLI: once-per-repo deterministic scan + candidates |
| `cmd/upgrade` | CLI: schedule upgrade runs from candidates |

## Phases

Progress is tracked per the plan's phases. Each phase ships, is tested (TDD),
and is committed before the next starts.

- [x] Phase 0 — spine (webhook + River + Postgres), no LLM
- [x] Phase 1 — deterministic scans (osv-scanner) → findings → actionable candidates
- [x] Phase 2 core — intent schema + validation kernel
- [x] Phase 2 core — executor (logic + go-github adapter, idempotent, PAT-only-held-here)
- [x] Phase 2 core — policy (dispatch-time grants) + runtime seam + registry + graph cache
- [ ] Phase 2 — LLM upgrade PR pipeline (machinery built: PI runtime via `--mode rpc` + persistent sessions, scheduler, worker wiring; live PR awaits write PAT + OpenRouter key on a persistent host)
- [ ] Phase 3 — PR review (read-only, fork-safe)
- [ ] Phase 4 — labeled issues (actor allowlist in policy ✅, worker wiring)
- [ ] Phase 5 — doc linting (taxonomy → redundancy → contradiction)

## Phase 2 upgrade pipeline

Deterministic scan candidates become draft upgrade PRs: the LLM agent runs
inside a sandboxed clone (PI via `--mode rpc`, under `tini`, with persistent
per-run sessions), bumps the dependency + migrates code off the default branch,
runs tests, and the executor pushes + opens a **draft** PR. The worker runs
without a job timeout (River's 1-minute default disabled); runpipe's own
40-minute wall-clock cap bounds each run. Secrets live in gitignored
`deploy/.env` (see `.env.example`) and only the executor resolves a write PAT.
Branch/PR base = repo default branch; draft-by-default + diff/path validation
+ human review are the safety net.

## Develop

```sh
export PATH=$HOME/.local/go-sdk/bin:$PATH   # this box ships Go outside $PATH
go test ./...
```
