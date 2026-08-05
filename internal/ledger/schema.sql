-- maintainer-bot audit spine. Idempotent; applied at every boot.
-- matches the data model in docs/MAINTAINER_BOT_PLAN.md §4.

-- runs is the audit spine: every webhook, and every PR the bot opens, is
-- traceable to a run, its grant, its inputs, and the graph it read.
CREATE TABLE IF NOT EXISTS runs (
    id             TEXT PRIMARY KEY,
    repo_id        TEXT NOT NULL,
    trigger        TEXT NOT NULL,            -- webhook_pull_request | webhook_issue_labeled | scheduled
    scope_kind     TEXT NOT NULL,            -- issue | pull_request | scheduled
    scope_number   INTEGER,
    "grant"        JSONB NOT NULL,
    decision       TEXT NOT NULL,            -- allow | deny
    deny_reason    TEXT,
    status         TEXT NOT NULL DEFAULT 'queued', -- queued|running|succeeded|failed|denied
    runtime        TEXT,
    model          TEXT,
    graph_key      TEXT,
    tokens_in      INTEGER,
    tokens_out     INTEGER,
    cost_usd       NUMERIC,
    failure_reason TEXT,
    meta           JSONB NOT NULL DEFAULT '{}', -- run-type inputs: upgrade {pkg,from,to,advisory} or scan payload
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at     TIMESTAMPTZ,
    ended_at       TIMESTAMPTZ
);
-- idempotent column additions for tables that may pre-date this schema
ALTER TABLE runs ADD COLUMN IF NOT EXISTS meta JSONB NOT NULL DEFAULT '{}';

-- intents: content-only agent output, resolved by the executor. The derived
-- idempotency key is unique so a River retry / GitHub redelivery cannot open
-- a duplicate PR.
CREATE TABLE IF NOT EXISTS intents (
    id              BIGSERIAL PRIMARY KEY,
    run_id          TEXT NOT NULL REFERENCES runs(id),
    kind            TEXT,                -- nullable: executor dedup path is idempotency-keyed
    payload         JSONB,
    idempotency_key TEXT UNIQUE NOT NULL,
    github_ref      TEXT,
    executed_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_intents_run ON intents(run_id);

CREATE TABLE IF NOT EXISTS findings (
    id              BIGSERIAL PRIMARY KEY,
    repo_id         TEXT NOT NULL,
    scanner         TEXT NOT NULL,           -- osv-scanner | trivy | govulncheck | renovate
    ecosystem       TEXT,
    package         TEXT NOT NULL,
    current_version TEXT,
    target_version  TEXT,
    severity        TEXT,
    advisory_id     TEXT,
    status          TEXT NOT NULL DEFAULT 'open',
    run_id          TEXT REFERENCES runs(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_findings_repo ON findings(repo_id);
-- A finding is identified by which vulnerability was found where; the upsert
-- key lets periodic scans refresh in place instead of duplicating rows.
CREATE UNIQUE INDEX IF NOT EXISTS idx_findings_dedup ON findings(repo_id, scanner, package, advisory_id);

-- updates: the Renovate dry-run "available-update set." One row per upgrade
-- Renovate resolves (current -> target). Joined against findings to surface
-- actionable upgrade candidates (a fix that is actually installable).
CREATE TABLE IF NOT EXISTS updates (
    id              BIGSERIAL PRIMARY KEY,
    repo_id         TEXT NOT NULL,
    ecosystem       TEXT,
    package         TEXT NOT NULL,
    current_version TEXT,
    target_version  TEXT,
    update_type     TEXT,   -- patch | minor | major | pin | digest
    run_id          TEXT REFERENCES runs(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_updates_repo ON updates(repo_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_updates_dedup ON updates(repo_id, ecosystem, package);

-- graph_cache: prebuilt codebase graphs keyed by the deterministic hash
-- (see internal/graphcache); a derived artifact, evictable by definition.
CREATE TABLE IF NOT EXISTS graph_cache (
    repo_id  TEXT NOT NULL,
    tree_sha TEXT NOT NULL,
    key      TEXT NOT NULL,
    built_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    bytes    BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (repo_id, tree_sha, key)
);
CREATE INDEX IF NOT EXISTS idx_graph_cache_key ON graph_cache(key);

-- capability_gaps: verb requests outside the closed set, recorded for weekly
-- human review — the deliberate-growth mechanism from the intent design.
CREATE TABLE IF NOT EXISTS capability_gaps (
    id             BIGSERIAL PRIMARY KEY,
    run_id         TEXT NOT NULL,
    requested_kind TEXT NOT NULL,
    context        TEXT,
    reviewed_at    TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- credentials: one fine-grained PAT per (resource owner, tier). The executor
-- resolves TokenScope -> credential from a keyring; the token itself never
-- touches Postgres.
CREATE TABLE IF NOT EXISTS accounts (
    id    TEXT PRIMARY KEY,
    login TEXT NOT NULL,
    kind  TEXT NOT NULL               -- org | user
);
CREATE TABLE IF NOT EXISTS credentials (
    id           TEXT PRIMARY KEY,
    account_id   TEXT NOT NULL REFERENCES accounts(id),
    tier         TEXT NOT NULL,        -- read_only | issues_write | contents_write
    secret_ref   TEXT NOT NULL,        -- keyring reference, never the token
    scoped_repos TEXT[],
    expires_at   TIMESTAMPTZ,
    rotated_at   TIMESTAMPTZ,
    UNIQUE (account_id, tier)
);

-- repo_config: per-repo go/no-go; policy reads the allowlists and deny list
-- from here.
CREATE TABLE IF NOT EXISTS repo_config (
    repo_id          TEXT PRIMARY KEY,
    account_id       TEXT NOT NULL,
    owner            TEXT NOT NULL,
    name             TEXT NOT NULL,
    default_branch   TEXT NOT NULL,
    enabled          BOOLEAN NOT NULL DEFAULT true,
    denied_paths     TEXT[] NOT NULL DEFAULT '{}',
    allowed_labels   TEXT[] NOT NULL DEFAULT '{}',
    actor_allowlist  TEXT[] NOT NULL DEFAULT '{}',
    budget_daily_usd NUMERIC,
    schedules        JSONB
);
