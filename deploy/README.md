# bothos spine migration wrapper

This directory holds the Docker Compose infrastructure for the bot's runtime.
`docker compose up -d` brings up Postgres; the gateway/worker/executor
containers get added per the plan's later phases.
