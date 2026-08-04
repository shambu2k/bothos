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
| *(planned)* `internal/policy` | OPA/grants — computes the immutable Grant at dispatch |
| *(planned)* `internal/executor` | Sole holder of GitHub PATs; resolves intents against GitHub |
| *(planned)* `internal/ledger` | `runs`/`findings` Postgres spine |
| *(planned)* `internal/runtime` | `AgentRuntime` interface — swappable agent backends |

## Phases

Progress is tracked per the plan's phases. Each phase ships, is tested (TDD),
and is committed before the next starts.

- [ ] Phase 0 — spine (webhook + River + Postgres), no LLM
- [ ] Phase 1 — deterministic scans (osv-scanner / trivy / renovate dry-run)
- [x] Phase 2 core — intent schema + validation kernel (this package)
- [ ] Phase 2 — executor + AgentRuntime behind dependency-upgrade workload
- [ ] Phase 3 — PR review (read-only, fork-safe)
- [ ] Phase 4 — labeled issues (actor allowlist)
- [ ] Phase 5 — doc linting (taxonomy → redundancy → contradiction)

## Develop

```sh
export PATH=$HOME/.local/go-sdk/bin:$PATH   # this box ships Go outside $PATH
go test ./...
```
