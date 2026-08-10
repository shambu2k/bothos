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
| `internal/runtime` | `AgentRuntime` seam + named runtime registry + security-remediation prompt |
| `internal/graphcache` | Deterministic graph cache keying + retention (`sha256(tool‖cfg‖tree)`) |
| `internal/ledger` | `runs`/`findings`/`updates` Postgres spine + actionable candidates |
| `internal/queue` | River-backed transactional queue + periodic jobs |
| `internal/scan` / `internal/scanjob` | Deterministic scanners (osv-scanner) → findings |
| `internal/agent` | PI agent via documented `--mode rpc` (persistent per-run sessions) + lifecycle hardening + external-verifier feedback loop |
| `internal/verifier` | Deterministic re-scan of the agent's claimed fixes (osv-scanner) + feedback rendering |
| `internal/upgrade` | Grant, scheduler, git-state helpers (branch/base resolution), git diff source |
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

A scheduled security run hands the agent a sandboxed clone and lets it own the
whole mechanical loop: scan (osv-scanner, with trivy available), triage, branch
(`bot/<runID>-*`), fix, commit, and self-verify — staged as a **draft** PR. The
Go bot owns only policy: the immutable dispatch grant, a deny-path diff gate,
an executor-only write token, and an external verifier that re-scans the
agent's claimed fixes with a bounded feedback loop (the agent cannot grade its
own homework). The executor reads branch and base from git state (the agent's
branch, the clone's `origin/HEAD`) — nothing targeting is ever transported
through the intent envelope, which eliminated the seam-bug class where branch
and base were derived in two different places. The worker runs without a job
timeout (River's 1-minute default disabled); runpipe's own 40-minute wall-clock
cap and the verifier's round limit bound each run. Secrets live in gitignored
`deploy/.env` (see `.env.example`); only the executor resolves a write PAT, and
it is stripped from the agent subprocess environment. Draft-by-default +
external verification + diff/path validation + human review are the safety net.

## Develop

```sh
export PATH=$HOME/.local/go-sdk/bin:$PATH   # this box ships Go outside $PATH
go test ./...
```
