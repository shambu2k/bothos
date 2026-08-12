# Bothos local deployment

Copy `.env.example` to `.env`, set the required values, then start the stack:

```sh
cp deploy/.env.example deploy/.env
docker compose -f deploy/docker-compose.yml up -d --build
curl -fsS http://localhost:8090/healthz
```

The stack runs:

- **Postgres** for the run ledger, scan findings, review-comment mapping, and
  River queue tables;
- **gateway** for signed GitHub webhook validation and policy dispatch; and
- **worker** for queued upgrade and pull-request review work.

See [`../docs/OPERATIONS.md`](../docs/OPERATIONS.md) for repository
configuration, webhook triggers, credentials, and run behavior.
