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
- `auto_review`: whether ordinary pull-request events initiate a review;
- `allowed_labels`: labels that may trigger labeled-issue work; and
- `actor_allowlist`: GitHub logins permitted to apply those labels.

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

Manual review commands require GitHub `write`, `maintain`, or `admin` permission
for the delivery actor. Applying the `bothos/review` pull-request label also
requires that actor to appear in the repository's `actor_allowlist`; an empty
list denies every label-triggered review. An exact `@bothos review` comment
continues to require GitHub permission only. A labeled-issue trigger requires
all three: an allowed label, an actor listed in that repository's
`actor_allowlist`, and that actor's GitHub `write`, `maintain`, or `admin`
permission. An empty `actor_allowlist` denies every labeled-issue trigger. The
gateway uses `GITHUB_READ_TOKEN` for permission lookup; a missing token or
failed lookup denies the trigger.

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

## Labeled-issue runs

For an allowed `issues.labeled` event, Bothos stores the webhook's issue title
and body as untrusted run metadata. The worker derives the issue number,
repository, and base branch again from the immutable grant, creates a local
`bot/<run-id>-*` branch, and accepts only one draft `open_pr` intent. The
executor still computes and validates the actual worktree diff before pushing.

A blocked agent verdict cannot choose a GitHub target or write kind. The
pipeline itself creates one grant-scoped issue comment explaining the blocker,
records its GitHub reference, and marks the run `needs_input`. No pull request
is opened. `needs_input` is terminal in the current deployment; a later label
creates a fresh run rather than resuming an old agent session.

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

Run status values are `queued`, `running`, `succeeded`, `failed`, `denied`, and
`needs_input`. A shutdown does not resume a partial agent session automatically.
To request a new review or issue attempt after a worker restart, use a new
manual trigger on the pull request or reapply an allowed issue label.
