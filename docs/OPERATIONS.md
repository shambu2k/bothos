# Operations

This document describes the behavior exposed by the current deployment.

## Services

Docker Compose starts three services:

| Service | Responsibility |
|---|---|
| `postgres` | Run ledger, scan findings, review-summary mapping, and River queue tables. |
| `gateway` | Validates signed GitHub webhooks, evaluates policy, records decisions, and enqueues allowed runs. Serves `GET /healthz`. |
| `worker` | Consumes queued runs, creates temporary worktrees, invokes PI, and executes validated GitHub operations. |

Start or rebuild the stack from the repository root:

```sh
docker compose -f deploy/docker-compose.yml up -d --build
curl -fsS http://localhost:8090/healthz
```

## Repository configuration

Each enabled repository has a `repo_config` row. The configuration supplies:

- `enabled`: whether Bothos may process triggers for the repository;
- `default_branch`: the branch used for scheduled upgrade work;
- `denied_paths`: additional paths that upgrade changes may not modify;
- `auto_review`: whether ordinary pull-request events initiate a review.

`auto_review` defaults to `false`. With the default setting, Bothos only reviews
when an authorized collaborator applies `bothos/review` or comments exactly
`@bothos review` on a pull request.

## Webhook handling

The gateway accepts `POST /webhook`. It verifies GitHub's signature before
parsing the event. A handled event always produces a ledger decision:

- an allowed event creates a queued run and River job in the same database
  transaction;
- a denied event creates a `denied` run and performs no GitHub action;
- unrelated events are acknowledged without creating a run.

Manual review labels and comments require GitHub `write`, `maintain`, or `admin`
permission for the delivery actor. The gateway uses `GITHUB_READ_TOKEN` for
this lookup; a missing token or failed lookup denies the manual trigger.

## Pull-request review

Manual review triggers:

- apply `bothos/review` to a pull request; or
- post a comment containing an exact `@bothos review` line.

For automatic reviews, set `auto_review=true` for the repository. Automatic
reviews handle pull-request `opened`, `reopened`, and `synchronize` events.

At dispatch, Bothos stores immutable base and head SHAs in the grant. The worker
fetches those commits, checks out the head detached, runs deterministic review
checks, and starts a read-only PI session. The review must leave the worktree
unchanged and may never submit an approval.

The executor posts:

1. inline comments through GitHub's pull-request comment API, using the stored
   head SHA, path, line, and side;
2. `[verified]` inline comments for deterministic checks with evidence;
3. `[opinion]` inline comments for model observations; and
4. one `<!-- bothos-pr-review -->` summary comment per repository/pull-request
   pair, updated in place on repeat reviews.

GitHub can reject an anchor when a changed line is no longer in the current
diff. A `422 Unprocessable Entity` for an individual inline comment is treated
as stale-anchor skipping; it does not prevent the remaining comments or summary
from being posted.

## Dependency scanning and remediation

`bothos-scan -repo OWNER/REPO` runs deterministic dependency scanning and stores
findings plus fixed-version candidates. It does not use an LLM or write to
GitHub.

`bothos-upgrade -repo OWNER/REPO -enabled` queues draft-remediation runs for
stored actionable candidates. The worker gives PI a temporary clone; PI creates
and commits a `bot/<run-id>-*` branch but cannot push it. Bothos verifies the
worktree and the executor pushes the branch and opens a draft pull request.

Bothos enforces a 40-minute agent wall-clock limit. A run whose grant has
expired is rejected before an agent session starts. Terminal failures are
recorded in the ledger and are not retried as a new model run.

## Credentials

Copy `deploy/.env.example` to `deploy/.env`. The files are gitignored.

| Variable | Used for |
|---|---|
| `GITHUB_WEBHOOK_SECRET` | GitHub webhook signature validation. |
| `GITHUB_READ_TOKEN` | Gateway permission and pull-request metadata lookups; authenticated cloning of private repositories for scans and reviews. |
| `GITHUB_COMMENT_TOKEN` | Pull-request review acknowledgements, inline comments, and persistent summary comments. |
| `GITHUB_WRITE_TOKEN` | Pushing upgrade branches and opening draft upgrade pull requests. |
| `OPENROUTER_API_KEY` / `PI_MODEL` | PI runtime model configuration. |

The agent subprocess removes all `GITHUB_WRITE_TOKEN*` and
`GITHUB_COMMENT_TOKEN*` variables. The read token is not a write credential and
is used when private repository access is required.

## Run ledger

`runs` records the trigger, policy decision, grant, status, failure reason, and
GitHub reference for every processed run. `review_comments` maps a pull request
to its persistent summary-comment ID. `findings` and `updates` retain
scan-derived dependency information.

Run status values are `queued`, `running`, `succeeded`, `failed`, and `denied`.
A shutdown does not resume a partial agent session automatically. To request a
new review after a worker restart, use a new manual trigger on the pull request.
