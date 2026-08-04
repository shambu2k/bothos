# maintainer-bot

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
| `internal/runtime` | `AgentRuntime` seam + injection-aware structured upgrade prompt |
| `internal/graphcache` | Deterministic graph cache keying + retention (`sha256(tool‖cfg‖tree)`) |
| *(planned)* `internal/ledger` | `runs`/`findings` Postgres spine |
| *(planned)* `internal/queue` | River-backed transactional queue + periodic jobs |
| *(planned)* `cmd/gateway` | Webhook receiver (sig validation, ack-fast, dispatch) |
| *(planned)* `cmd/worker` | Run orchestration: sandbox → runtime → executor |
| *(planned)* `cmd/executor` | Executor container entrypoint |

## Phases

Progress is tracked per the plan's phases. Each phase ships, is tested (TDD),
and is committed before the next starts.

- [ ] Phase 0 — spine (webhook + River + Postgres), no LLM
- [ ] Phase 1 — deterministic scans (osv-scanner / trivy / renovate dry-run)
- [x] Phase 2 core — intent schema + validation kernel
- [x] Phase 2 core — executor (logic + go-github adapter, idempotent, PAT-only-held-here)
- [x] Phase 2 core — policy (dispatch-time grants) + runtime seam + graph cache
- [ ] Phase 2 — worker orchestration + real sandbox bound in, 10 upgrade PRs exit
- [ ] Phase 3 — PR review (read-only, fork-safe)
- [ ] Phase 4 — labeled issues (actor allowlist in policy ✅, worker wiring)
- [ ] Phase 5 — doc linting (taxonomy → redundancy → contradiction)

## Develop

```sh
export PATH=$HOME/.local/go-sdk/bin:$PATH   # this box ships Go outside $PATH
go test ./...
```
