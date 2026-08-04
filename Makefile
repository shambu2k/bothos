GO ?= go

# The DB-backed packages (ledger, queue, dispatch, cmd/gateway) share one
# integration database, so packages MUST run serially (-p 1) — otherwise
# concurrent TRUNCATEs deadlock and leftover River jobs cross-contaminate.
test:
	$(GO) test -count=1 -p 1 ./...

# Same as test, with coverage.
cover:
	$(GO) test -count=1 -p 1 -cover ./...

vet:
	$(GO) vet ./...

build:
	$(GO) build ./...

# Start the Phase 0 spine (Postgres) and apply migrations on boot.
up:
	docker compose -f deploy/docker-compose.yml up -d

.PHONY: test cover vet build up
