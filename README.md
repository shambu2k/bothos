# bothos

Self-hosted GitHub maintainer automation. Bothos records webhook decisions in
Postgres, queues allowed work through River, runs a PI agent in a temporary
git worktree, and performs GitHub writes only after validating a scoped intent.

- [Operations and configuration](docs/OPERATIONS.md)
- [Security design](docs/DESIGN.md)
- [Local deployment](deploy/README.md)

## Implemented workloads

### Deterministic dependency scanning

`bothos-scan` clones one repository, runs `osv-scanner`, stores findings and
available fixed versions in the ledger, and reports actionable candidates. It
does not call a model or write to GitHub.

```sh
bothos-scan -repo OWNER/REPO
```

### Draft dependency-remediation pull requests

`bothos-upgrade` queues remediation runs for actionable candidates. A worker
creates a temporary clone, and the PI runtime investigates, changes,
self-verifies, and commits on a `bot/<run-id>-*` branch. Bothos then validates
the worktree diff and uses the write credential to push the branch and open a
draft pull request. A deterministic verifier re-scans claimed fixes and can
feed bounded feedback to the same agent session before the pull request is
opened.

```sh
bothos-upgrade -repo OWNER/REPO -enabled
```

### Labeled issues

An enabled repository may configure one or more allowed issue labels and one or
more permitted GitHub actors in `repo_config.actor_allowlist`. Bothos records
and queues an issue run only when an allowlisted label is applied by an actor
who is **both** listed in that configuration and holds GitHub `write`,
`maintain`, or `admin` permission. An empty actor list denies every
labeled-issue trigger. The membership and permission lookups fail closed. The
issue title and body are passed to PI as untrusted context; the repository,
issue number, base branch, and write capability stay fixed by the immutable
grant.

The worker creates a local `bot/<run-id>-*` branch, asks PI for the smallest
complete fix, and opens exactly one draft pull request only after the standard
intent and worktree checks pass. If the agent cannot safely complete the work,
Bothos posts a trusted handoff comment on the original issue, stores the blocker
reason, and finishes the run as `needs_input`; no pull request is opened.

A `needs_input` handoff does not resume automatically. After providing the
requested answer, apply an allowed label again to create a fresh run.

### Pull-request review

Reviews are opt-in by default. They can be triggered by either applying the
`bothos/review` label or posting a comment containing exactly:

```text
@bothos review
```

Repositories can instead enable automatic reviews for pull-request opened,
reopened, and synchronize events with `repo_config.auto_review=true`.

Each review uses the base and head commit SHAs captured at dispatch. The worker
runs deterministic diff checks and a read-only PI review. Results are posted as:

- real inline GitHub pull-request comments, anchored to the current diff;
- `[verified]` findings backed by deterministic evidence;
- `[opinion]` observations from the model; and
- one marker-backed summary comment that is updated on later reviews of the
  same pull request.

Bothos never submits a GitHub approval. A model recommendation to request
changes is rendered as opinion text, not as a review-state change. If GitHub
rejects an inline comment because its line is no longer in the diff, Bothos
skips that stale anchor and still updates the summary.

## Core safety properties

- GitHub events are signature-validated before dispatch.
- Policy creates an immutable per-run grant before the agent starts.
- The agent emits content-only intents; repository, pull request, branch, and
  base targeting are derived from the grant or local git state.
- The executor validates the intent again immediately before a GitHub write.
- Upgrade runs are draft-only, and default denied paths prevent changes to
  workflow, registry, ownership, and bot configuration files.
- Manual review triggers require the actor to have GitHub write, maintain, or
  admin permission; an unavailable permission lookup fails closed.
- The agent process does not receive write or comment credentials. A read token
  may be available for authenticated private-repository cloning.

## Development

```sh
export PATH=$HOME/.local/go-sdk/bin:$PATH
go test -count=1 -p 1 ./...
```
