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
| Agent | `AgentRuntime` iface — Pi via `--mode rpc` (implemented) / OpenHands agent-server / Claude Agent SDK | Swappable |
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

### 5.2 Security-remediation PRs (harness around the agent)

A scheduled run queues **one run per repo** (not per finding). The harness gives
the agent a sandboxed clone and lets it own the whole mechanical loop: scan
(`osv-scanner`, with `trivy` available), triage, branch (`bot/<runID>-*`), fix,
commit, and self-verify. The Go bot owns only policy — it no longer mints a
candidate for the agent to execute.

The agent reports a structured verdict (`.bothos/verdict.json`, one of `done` /
`done_unverified` / `blocked`, plus an optional `fixes` list naming the
dependencies it claims to have upgraded). The harness holds the session open,
reads the verdict, nudges once if it is missing, then runs an **external
verifier** that deterministically re-scans the committed worktree with
`osv-scanner`: it checks the worktree is committed, that each claimed fix is
actually gone, and runs the repo's conventional test command. Failures are fed
back as prompts for another round, bounded by a round limit and a stall detector
(same failure signature twice → stop). The agent cannot grade its own homework.

- **Blocked** verdict → stand-down: no PR, the run is recorded failed with the
  reason (`failure_reason`), and it is **not** retried by River.
- **Verifier-red but committed** → still opens a **draft** PR whose body carries
  a `Known failures (external verifier)` section. A run is never silently
  presented as clean.
- Unverified or nil verdict → draft PR with the agent's report, clearly marked.

**Targeting comes from git state, never the envelope.** The agent's branch is
read from git (`rev-parse --abbrev-ref HEAD`), the base from the clone's
`origin/HEAD`, and the worktree is passed to the executor out-of-band. Nothing
that answers "where does this land" is transported through the intent payload —
this eliminated the seam-bug class where branch and base were derived in two
different places and the worktree path was smuggled through the envelope.

**Agent runtime (implemented).** The default runtime is PI via its documented
`--mode rpc` subprocess, wrapped by the swappable `AgentRuntime` seam
(`internal/agent`). Lifecycle engineering that mattered:

- The worker runs under a real init (`tini`) so the agent child is properly
  supervised (orphan adoption, signal forwarding, zombie reaping).
- Each run gets its own **process group** (`Setpgid`) and a **persistent
  per-run session** (`--session-dir` on a mounted volume) so sessions survive
  restarts and are recoverable.
- Shutdown is graceful: on cancellation the process receives `SIGTERM`, bounded
  by a `WaitDelay`, before any hard kill — never an abrupt `SIGKILL`.
- The deterministic `open_pr` intent is built in Go *after* the agent finishes;
  the agent contributes content only, the executor supplies all targeting.
- The RPC session stays open across a **settle → verdict → verifier-feedback**
  cycle: after the agent settles, runpipe reads `.bothos/verdict.json`, nudges
  once if it is missing, and runs the external-verifier loop described above.
- **Env hygiene:** the executor's write PAT is stripped from the agent
  subprocess environment (`withoutSecrets` drops every `GITHUB_WRITE_TOKEN*`).
  The sandbox holds no write credential, so exfiltration-by-push is closed.

**River gotcha (bit us in production).** River's default job timeout is
**1 minute**. Every long agent run was killed at ~60s — surfacing as a killed
subprocess, and only identifiable once the runtime logged the context cause.
The queue client therefore sets `JobTimeout: -1` and relies on runpipe's own
40-minute wall-clock cap (and token limits) to bound each run.

### 5.3 PR review

**Status: implemented; Phase 3 exit evidence still requires the live sample
below.** Reviews are read-only and opt-in:

1. Label `bothos/review` on a PR.
2. Comment `@bothos review` on a PR.
3. Set `repo_config.auto_review=true` for automatic review on
   `pull_request` opened, reopened, or synchronize events.

`repo_config.auto_review` is deployment-owned and defaults to `false`.
Unconfigured repositories and configured repositories with the flag off do
not auto-review. Label and mention requests override that flag, but both
require the delivery actor to have GitHub collaborator write permission.
Permission lookup is authenticated with `GITHUB_READ_TOKEN` and fails closed
when the token is absent or the GitHub API fails. `allowed_labels` is reserved
for the Phase 4 issue workflow; it is not consulted for `bothos/review`.

Configure a scratch repository explicitly:

```sql
INSERT INTO repo_config (
    repo_id, account_id, owner, name, default_branch, enabled,
    denied_paths, allowed_labels, actor_allowlist, auto_review
) VALUES (
    'OWNER/REPO', 'ACCOUNT_ID', 'OWNER', 'REPO', 'main', true,
    '{}', '{}', '{}', false
)
ON CONFLICT (repo_id) DO UPDATE SET
    account_id = EXCLUDED.account_id,
    owner = EXCLUDED.owner,
    name = EXCLUDED.name,
    default_branch = EXCLUDED.default_branch,
    enabled = EXCLUDED.enabled,
    auto_review = EXCLUDED.auto_review;
```

Set the final value to `true` only when automatic review is intended. Manual
label/mention review works while it remains `false`.

The worker checks out the immutable base/head SHAs recorded in the grant.
Fork and private-PR fetches are tokenless inside the review sandbox: no read,
comment, or contents-write PAT enters the agent process. If Git cannot fetch
the granted head without a token, the run fails rather than reviewing a
different ref. Repository files and instructions, including `AGENTS.md`, are
untrusted review context; they cannot widen the grant or direct bot behavior.

Every emitted item has a visible provenance tag:

- `[verified]` rules are `denied_path`, `secret`, `dependency_delta`,
  `lockfile_only`, and `osv_delta`. Each item carries its rule, detail,
  `path:line`, and bounded deterministic evidence. `osv_delta` evidence names
  the advisory and `osv-scanner --format json .`; the other rules preserve the
  relevant diff header or added line.
- `[opinion]` covers the model summary and every model review comment. Model
  output cannot mark itself verified; classification is added only after the
  deterministic checks complete.

The bot never submits a GitHub `APPROVE` event. A request-changes verdict is
rendered only as an `[opinion]` recommendation. The executor uses a
comment-only `GITHUB_COMMENT_TOKEN` to create or update one
`<!-- bothos-pr-review -->` issue comment per `(repo, PR)`. The comment ID is
stored in `review_comments` and reused for acknowledgements, retries, and new
heads; recovery accepts the marker only when the authenticated bot owns it.

### 5.4 Labeled issues → PR

Widest attack surface: whoever can apply a label can trigger a write-capable
run. Two gates, both in policy:

1. Label ∈ `repo_config.allowed_labels`
2. Actor ∈ `repo_config.actor_allowlist` (or has write permission on the repo,
   resolved via GitHub collaborator permission at dispatch — fail closed
   without a token; never default-allow)

Ship this **last**.

**Lifecycle.** Label `bothos/fix` on an issue → bot reacts 👀 and comments
"on it" → agent clones, reads the issue itself, fixes → opens a **draft PR**
whose body is the verifier **receipt** (scan before/after, tests, claimed
fixes, "not touched" denied paths, `Fixes #N` attached by the executor from
scope — never from agent text) → delivery comment with summary + PR link →
**label removed** (the label is a queue: re-add = re-run; close-without-merge
+ re-add = retry). One live run per issue (dedupe on redelivery/resume).

**Handoff, not failure (differentiator).** When the agent is blocked it does
not die silently and does not invent an answer. It posts one handoff comment —
what it tried, the blocker as *a question a human can answer in ~30 seconds*,
the options it sees — park the run as **`needs_input`**, and keep the label on.
A human reply (mention or plain answer to a parked run) spawns a **resume run**
linked to the parent (`parent_run_id`): the agent continues its own PI session
with the answer in context and proceeds to the PR. Blocked runs are never
River-retried; they wait for a human.

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

**Phase 3 — PR review (implemented; live exit sample pending).**
Read-only grant, tokenless sandbox checkout, and opt-in dispatch via label
`bothos/review`, `@bothos review`, or deployment-owned per-repo `auto_review`.
Tier-1 deterministic rules emit `[verified]`; model prose emits `[opinion]`.
One marker-backed comment is updated in place, and no approval event exists.
*Exit:* review comments on 20 real PRs with five verified items manually
reproduced, no `APPROVED` review, one comment ID per reviewed PR, no write token
in any sandbox, and a false-positive rate you'd tolerate as a reviewer. If you
wouldn't tolerate it, tune the prompt before proceeding.

**Phase 4 — labeled issues.**
Actor allowlist enforced in policy, `ActorHasWrite` resolved from GitHub
(fail closed). Label `bothos/fix` → draft PR with verifier receipt, label
removed on done. Blocked → handoff question comment + `needs_input` + resume
on human reply.
*Exit:* an unauthorized actor applying the trigger label produces a denied run in
the ledger and nothing else; a blocked run always yields a handoff comment
(never a silent stand-down) and resumes correctly on a reply.

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
| Agent runtime | Pi via `--mode rpc` (implemented); OpenHands agent-server as second impl | Whichever wins on upgrade-PR success rate in Phase 2 |
| Local vs API model | Hybrid, API for code-writing | Cost per merged PR |
| pgvector | Drop | Docs-redundancy check, or run-history search |
| `globMatch` | Placeholder | Before the deny list is load-bearing — swap in a real gitignore matcher |
| PAT rotation | Manual, 90-day | If repo count grows past what one rate-limit budget serves |

Measure Phase 2's success rate before committing to a runtime. That single
number — upgrade PRs that pass tests and get merged without edits — should drive
most of the remaining choices.
