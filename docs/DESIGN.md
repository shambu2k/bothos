# Security design

Bothos treats repository content and GitHub event content as untrusted. Pull
request descriptions, comments, issue bodies, source files, advisory text, and
repository instructions can be included in an agent's context, but they cannot
change a run's authority.

## Immutable grants

The gateway evaluates repository policy before it queues work. An allowed event
creates an immutable grant containing the repository, scope, allowed intent
kinds, denied paths, token scope, base/head commit SHAs where applicable, and
an expiry. The grant is stored with the run.

The agent does not receive the grant. It receives only a task, a temporary
git worktree, and runtime limits. It returns content-only intents. Before any
GitHub write, the executor validates the intent against the original grant
again and derives all placement from the grant or local git state.

This prevents agent-controlled text from changing the target repository,
pull-request number, base branch, or work branch.

## Review controls

A `bothos/review` label grant is created only when the label sender is both
listed in `actor_allowlist` and verified by GitHub to have `write`, `maintain`,
or `admin` repository permission. The exact `@bothos review` comment path
retains its GitHub permission check but does not use the label-actor list. A
review grant permits only `post_review`. It is scoped to one pull request and
to the base and head SHAs captured during dispatch. The worker verifies that the
checked-out head matches the grant and that the review worktree remains clean.

Bothos has no approval capability. It posts inline comments and a persistent
summary comment; it does not submit a GitHub approval or request-changes event.
Deterministic findings are labeled `[verified]` and include evidence. Model
observations are labeled `[opinion]`.

## Labeled-issue controls

A labeled-issue grant is created only when the webhook label matches
`allowed_labels` and its sender is both listed in `actor_allowlist` and verified
by GitHub to have `write`, `maintain`, or `admin` repository permission. An
empty actor allowlist denies all labeled-issue events. A grant permits `open_pr`
and `post_comment` only for its one issue scope. The webhook title and body are
persisted solely as untrusted agent context. Before the agent starts, the worker
rebuilds repository, issue number, and base branch from the grant; persisted
metadata cannot retarget the task.

Success accepts exactly one draft pull-request intent. A blocked verdict does
not grant the agent a comment capability of its choosing: trusted pipeline code
constructs one grant-scoped handoff comment, records the blocker, and marks the
run `needs_input`. That state is terminal and creates no pull request.

## Upgrade controls

An upgrade grant permits only draft-pull-request work. The agent can create and
commit on a run-specific local branch, but it cannot push. Before pushing or
opening a pull request, Bothos computes the actual worktree diff and rejects
changes that touch the default denied paths or repository-specific denied
paths. Default denied paths cover workflow, registry, ownership, and bot
configuration files.

The external verifier re-scans claimed dependency fixes and reports its result
before the draft pull request is created.

## Credential handling

Credentials are supplied from environment variables that are excluded from git.
The worker resolves the credential required for a validated operation only at
the write boundary. The PI subprocess removes every `GITHUB_WRITE_TOKEN*` and
`GITHUB_COMMENT_TOKEN*` environment variable. A read token can be present for
private-repository cloning and authenticated metadata lookups; it is not a
write credential.

## Idempotency and failures

GitHub writes use derived idempotency keys. The ledger records successful writes
so a repeated delivery does not duplicate an already-executed operation. Review
summary comments are additionally mapped by repository and pull-request number
so the same marker-backed comment can be updated on a later review.

Failed agent runs are recorded as terminal failures. They are not retried as
new model runs automatically.
