# Bothos local deployment

Copy `.env.example` to `.env`, set the required credentials, then run
`docker compose up -d`. The stack starts Postgres, the signed-webhook gateway,
and the worker; credentials are split by service as documented in the example.
