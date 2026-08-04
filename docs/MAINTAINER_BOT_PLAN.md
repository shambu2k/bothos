# GitHub Maintainer Bot — Build Plan

Self-hosted, homelab-deployed, per-repo AI maintainer.

---

## 1. Scope

**In scope**

| Workload | Trigger | Writes |
|---|---|---|
| Recurring security audit | Cron | Findings only |
| Dependency upgrade PRs, incl. code migration | Finding | PR |
| PR review | Webhook (`pull_request`) | Review comments |
| Small issues → PR | Webhook (`issues.labeled`) | PR |
| Documentation linting — taxonomy, redundancy, contradiction | Cron | PR or issue |

**Out of scope for v1** — CI repair (deferred; it needs write access to a
running build's context, which is a different and larger security surface),
multi-tenant hosting, non-GitHub forges.

**Relationship to the runtime product.** This is the application. The
`AgentRuntime` interface is the seam to the governance-wrapper product; keep it
narrow and don't let maintainer-bot concerns leak across it. The bot should not
know which runtime it's running on, and the runtime should not know it's serving
a maintainer bot.

---

## 2. Architecture

```
org/repo webhook ──> gateway (go-github ParseWebHook)
                            │  validate sig, ack <10s
                            ▼
                        policy eval (OPA) ──deny──> ledger, drop
                            │ allow → Grant
                            ▼
                        River.InsertTx  ──┬── q:scan
                                          ├── q:upgrade
                                          ├── q:review
                                          ├── q:issue-pr
                                          └── q:doc-lint
                            ▼
                        worker
                            │  1. deterministic scanners
                            │  2. resolve graph (cache by tree SHA)
                            │  3. AgentRuntime.Run(task, sandbox)
                            ▼
                        intents (no token held)
                            ▼
                        executor ── validate ──> GitHub API
                        (separate process; sole holder of the PATs)
```

**Process boundaries that matter.** The executor is its own container, not a
package the workers import. It is the only process holding any GitHub
credential. Workers talk to it over a local socket. If a worker is compromised
through its sandbox, it has nothing to steal — no token is ever present in a
worker's environment or in a run's sandbox.

---

## 3. Reuse map

| Layer | Component | Notes |
|---|---|---|
| Webhook ingress | `google/go-github` (`ValidatePayload` + `ParseWebHook`), optionally `cbrgm/githubevents` for typed routing | `go-githubapp` is App-auth centric and not usable here; you own the ack-fast/dispatch-async logic instead |
| GitHub API | `google/go-github` + `githubv4` | v4 for anything paginated and relational |
| Queue + scheduling | `riverqueue/river` | Postgres-backed, transactional enqueue, periodic jobs, unique-by-args, web UI |
| Policy | OPA (or Cedar) | Grants as data |
| Vulnerabilities | `osv-scanner`, `trivy`, `govulncheck` | Deterministic; no tokens spent |
| Update resolution | Renovate `--dry-run` | Don't reimplement version resolution across ecosystems |
| SBOM | `syft` / `grype` | Optional; useful for the audit report |
| Codebase context | Graphify | Tree-sitter, local, Apache 2.0, MCP server |
| Sandbox | gVisor (runsc) under Docker | Ephemeral per run |
| Agent | `AgentRuntime` iface — Pi SDK / OpenHands agent-server / Claude Agent SDK | Swappable |
| Telemetry | OpenTelemetry + Langfuse | Per-run trace, tokens, cost |
| Egress control | Squid or similar, allowlist | Registry hosts + git remote only |

**Written from scratch:** policy kernel, intent schema and executor, run ledger,
injection containment. That's the product; everything above is plumbing.

---

## 4. Data model

```
accounts        (id, login, kind)                -- resource owner: org or user
credentials     (id, account_id, tier, secret_ref, scoped_repos[],
                 expires_at, rotated_at)          -- one row per PAT
repos           (id, account_id, owner, name, default_branch, enabled)
repo_config     (repo_id, denied_paths[], allowed_labels[], actor_allowlist[],
                 budget_daily_usd, schedules jsonb)

runs            (id, repo_id, trigger, scope_kind, scope_number, grant jsonb,
                 runtime, model, graph_key, status, tokens_in, tokens_out,
                 cost_usd, started_at, ended_at, failure_reason)

intents         (id, run_id, kind, payload jsonb, idempotency_key UNIQUE,
                 validation jsonb, executed_at, github_ref)

findings        (id, repo_id, scanner, ecosystem, package, current_version,
                 target_version, severity, advisory_id, status, run_id)

graph_cache     (repo_id, tree_sha, key, built_at, bytes)
capability_gaps (run_id, requested_kind, context, reviewed_at)
river_*         (queue tables)
```

`runs` is the audit spine. Every PR the bot opens is traceable to a run, its
grant, its inputs, and the exact graph it read. Note `capability_gaps` — that's
the deliberate-growth mechanism from the intent design.

---

## 5. Workloads

### 5.1 Security audit (cron)

River periodic job per repo. Shallow clone into sandbox → run
`osv-scanner`/`trivy`/`govulncheck` → upsert `findings`. **No LLM.** Output is a
table, not prose. Optionally one summary comment on a standing dashboard issue.

Renovate `--dry-run` runs alongside, producing the available-update set. Join it
against findings: a vulnerability with a known fixed version becomes an
actionable upgrade candidate.

### 5.2 Dependency upgrade PRs (with migration)

One finding → one run → one PR. Never batch; a failed batch is undiagnosable.

Agent input is structured, not "go find problems":

```
package, current version, target version,
changelog/release notes between them,
graph nodes referencing the package,
test command
```

The agent's job is the migration and getting tests green — discovery already
happened deterministically. Success criterion is the repo's own suite inside the
sandbox. Tests pass → normal PR. Tests fail → **draft** PR with the agent's
notes and the failing output. Never hide a failure behind a clean-looking PR.

### 5.3 PR review

`pull_request` opened/synchronize → read-only grant, `q:review`. The sandbox
executes untrusted fork code, so this run holds no write-capable token at all;
review comments come back as intents and the executor posts them.

`graphify prs` gives merge-conflict risk by graph community, which is worth
surfacing alongside the review.

No approve verdict — see the intent schema.

### 5.4 Labeled issues → PR

Widest attack surface: whoever can apply a label can trigger a write-capable
run. Two gates, both in policy:

1. Label ∈ `repo_config.allowed_labels`
2. Actor ∈ `repo_config.actor_allowlist` (or has write permission on the repo)

Ship this **last**.

### 5.5 Documentation linting

Cron, over the docs corpus. Three checks, and they're genuinely different jobs:

- **Taxonomy** — headings/sections against the repo's own information
  architecture. Mostly rule-based; drift from a declared structure.
- **Redundancy** — same concept documented in two places. This is the one place
  embeddings would earn their keep, over the *docs corpus only*, not the code.
- **Contradiction** — two docs asserting incompatible things. Needs an LLM and
  is the highest false-positive risk of anything here.

Start with taxonomy (cheap, deterministic, obviously right or wrong). Add
contradiction only once you have a tolerance for noise, and open issues rather
than PRs for it — a contradiction usually needs a human to decide which side is
wrong.

---

## 6. Security model

**Intent boundary.** The agent emits content-only intents; the executor supplies
all targeting. See `intent/intent.go` and `DESIGN.md`.

**Grants** are computed by policy at dispatch, before the agent runs, and are
immutable for the run. Nothing the agent reads can widen its own capability.

**Token scoping.** Three tiers — `read_only`, `issues_write`, `contents_write` —
mapped from intent kind. Review runs never exceed `read_only`.

With a machine account rather than an App, a tier is a **separate fine-grained
PAT**, not a minted token. Issue three per resource owner, each scoped to the
same repo set with escalating permissions, and store them separately. The
executor resolves `TokenScope` → credential from a keyring; the enum and every
validation rule are unchanged. Never issue a classic PAT: a classic token can
reach every repository the account can reach, and orgs can refuse them outright
with a 403.

**Sandbox.** gVisor runtime, ephemeral container per run, no Docker socket, tmpfs
workspace, egress via allowlist proxy (package registries + `github.com` only).
No GitHub token is ever forwarded into the sandbox environment.

**Injection containment.** Untrusted text (issue bodies, PR descriptions,
changelogs, advisories) is tagged as data in the prompt and recorded in
`intents.sources`. Containment is structural, not prompt-level: the capability
set was fixed before the text was read.

**Fork PRs.** Reviewed in a sandbox with no write token. If you later add
self-hosted GitHub Actions runners for anything, keep them off public repos —
fork PRs execute arbitrary code on your box.

### Credential model (machine account, no GitHub App)

The bot authenticates as a real GitHub user account holding fine-grained PATs.
Everything above — grants, tiers, the intent boundary — is unchanged. What
changes is operational.

**Webhooks are yours to configure.** No installation event, no auto-subscribe.
Use an **org-level webhook** where possible so one endpoint covers every repo;
fall back to per-repo hooks otherwise. Same shared secret, validated in the
gateway. Adding a repo becomes a provisioning step, so make it scripted.

**One PAT per resource owner.** A fine-grained PAT targets a single resource
owner — one org, or the user. Multi-org coverage means a credential set per org,
which is why `credentials` hangs off `accounts` rather than being global.

**Rate limit is the real cost.** Installation tokens get their own budget per
installation; a user PAT spends one 5,000/hr budget for everything the account
does, across all repos. Mitigations, in order of payoff:
- Conditional requests with ETags — a 304 doesn't spend budget
- GraphQL for relational reads (one call instead of N)
- Cache repo metadata, labels, and file trees in Postgres; refresh on webhook
- Respect secondary limits on writes; serialize PR creation per repo

Budget headroom is a real constraint on how many repos this bot can serve.
Measure it in Phase 0 before you assume a number.

**Rotation is manual.** There is no API to rotate a PAT. Expiry options are
7/30/60/90 days, a custom value up to a year, or none — and orgs default to a
366-day maximum. Pick 90 days, put rotation in the calendar, alert on
`credentials.expires_at`, and make sure the machine account's email is monitored
by a human, because that's where the expiry warning lands.

**The account is load-bearing.** A PAT is tied to the user who created it and
goes inactive if that user loses access to the resource. So: 2FA on with
recovery codes stored, SSO-authorize the tokens if the org uses SAML, and don't
let the account drift out of the org. On paid orgs it likely consumes a seat.

**Blast radius, honestly.** This is the one place the App design was strictly
better: an App key mints 1-hour tokens and an installation can be revoked
centrally. A PAT is usable the moment it leaks, until someone rotates it. Fine-
grained scoping, per-tier separation, 90-day expiry, and executor-only storage
narrow that meaningfully — but the credential in your executor is now the
crown jewel, and it should live in a secrets manager, not an env var in a
Compose file.

---

## 7. Context layer

Graphify, code-only corpus (skip the docs/PDF/image pass — it routes through a
model and costs tokens per build).

Cache key: `sha256(graphify_version || extraction_config || tree_sha)`. Tree SHA
rather than commit SHA so rebases and no-op merges share a graph. Build on
default-branch push via a River job unique by key. Graph build failure degrades
the run, never fails it.

**No pgvector for code context.** Graph traversal plus ripgrep gives exact,
citable relationships; a vector index would be a second weaker index of the same
thing plus cross-run state. Reconsider only for (a) the docs-redundancy check
above, or (b) semantic search over your own run history — both are indexes over
different corpora than the codebase.

---

## 8. Deployment (homelab)

Docker Compose:

| Service | Holds |
|---|---|
| `gateway` | Webhook secret only. No GitHub token. |
| `worker` × N | Nothing. Talks to executor over unix socket. |
| `executor` | All PATs. Sole GitHub write path. |
| `graph-builder` | Nothing |
| `postgres` | Run ledger, findings, River, graph index |
| `proxy` | Egress allowlist |
| `otel-collector` | — |

Ingress: Cloudflare Tunnel or Tailscale Funnel to `gateway` only. Don't
port-forward.

Model routing: local model for triage, classification, and severity ranking;
API model for anything that writes code. Multi-file dependency migration is
where small local models degrade hardest, and it's exactly the workload where a
wrong answer costs you a bad PR.

---

## 9. Phases

Each phase ships and runs before the next starts.

**Phase 0 — spine, no LLM.**
Webhook receiver + River + Postgres. Every webhook lands in `runs` with its policy
decision. Workers log and exit.
*Exit:* one week of real traffic recorded, zero dropped deliveries, River UI up.

**Phase 1 — deterministic scans.**
Periodic scan job, scanners in sandbox, `findings` populated. Renovate dry-run
joined in.
*Exit:* findings table matches a manual audit on two repos. You now have a
useful security bot at zero LLM cost.

**Phase 2 — first write path: dependency upgrades.**
`AgentRuntime` behind one workload. Intent schema and executor live.
*Exit:* 10 consecutive upgrade PRs, none touching a denied path, none opened
twice on retry, every one traceable to a run.

**Phase 3 — PR review.**
Read-only grant, fork-safe.
*Exit:* review comments on 20 PRs with a false-positive rate you'd tolerate as a
reviewer. If you wouldn't, tune before proceeding — a noisy bot gets muted.

**Phase 4 — labeled issues.**
Actor allowlist enforced in policy.
*Exit:* an unauthorized actor applying the trigger label produces a denied run in
the ledger and nothing else.

**Phase 5 — doc linting.**
Taxonomy first. Redundancy second. Contradiction only if the first two are quiet.

---

## 10. Controls

- Per-run token cap and wall-clock cap, enforced in `AgentRuntime`
- Per-repo daily budget in `repo_config`; exceeded → runs queue, don't fail
- Circuit breaker: N consecutive failed runs on a repo disables it and files an
  issue
- Concurrency limits per queue so review can't starve upgrades
- `capability_gaps` reviewed weekly — that's your backlog of verbs to consider

---

## 11. Open decisions

| Question | Current lean | Trigger to revisit |
|---|---|---|
| Intents vs scoped token | Intents | If the verb set exceeds ~10 and keeps growing |
| PR-head graph | Base graph only | Instrument how often the agent misses PR-introduced symbols |
| Agent runtime | Pi SDK, OpenHands agent-server as second impl | Whichever wins on upgrade-PR success rate in Phase 2 |
| Local vs API model | Hybrid, API for code-writing | Cost per merged PR |
| pgvector | Drop | Docs-redundancy check, or run-history search |
| `globMatch` | Placeholder | Before the deny list is load-bearing — swap in a real gitignore matcher |
| PAT rotation | Manual, 90-day | If repo count grows past what one rate-limit budget serves |

Measure Phase 2's success rate before committing to a runtime. That single
number — upgrade PRs that pass tests and get merged without edits — should drive
most of the remaining choices.
