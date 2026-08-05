# Multi-stage build for the bothos spine containers.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# Deterministic scanners (run against shallow clones in the worker/scan
# container). Renovate is intentionally absent (dry-run report-gen deferred).
RUN go install golang.org/x/vuln/cmd/govulncheck@latest \
 && go install github.com/google/osv-scanner/v2/cmd/osv-scanner@latest
COPY . .
RUN CGO_ENABLED=0 go build -o /out/gateway ./cmd/gateway \
 && CGO_ENABLED=0 go build -o /out/worker ./cmd/worker \
 && CGO_ENABLED=0 go build -o /out/scan ./cmd/scan \
 && CGO_ENABLED=0 go build -o /out/upgrade ./cmd/upgrade

# Runtime: Node base so the worker can run the PI Agent sidecar (the Phase 2
# LLM runtime) and `npm` for npm-ecosystem build/test during upgrades, plus the
# static Go binaries and standalone scanners. A Go toolchain is NOT included;
# Go-ecosystem upgrades need it added later if used.
FROM node:20-alpine
RUN apk add --no-cache ca-certificates git

# PI sidecar adapter + its deps (installed before COPY admin so the layer caches).
COPY adapter/pi/package.json /opt/bothos/pi/package.json
RUN cd /opt/bothos/pi && npm install --no-audit --no-fund --omit=dev
COPY adapter/pi /opt/bothos/pi

COPY --from=build /go/bin/govulncheck /usr/local/bin/govulncheck
COPY --from=build /go/bin/osv-scanner /usr/local/bin/osv-scanner
COPY --from=build /out/gateway /usr/local/bin/bothos-gateway
COPY --from=build /out/worker /usr/local/bin/bothos-worker
COPY --from=build /out/scan /usr/local/bin/bothos-scan
COPY --from=build /out/upgrade /usr/local/bin/bothos-upgrade

# Launcher used by the worker as -pi-adapter (default resolves to this).
RUN printf '#!/bin/sh\nexec node /opt/bothos/pi/adapter.mjs "$@"\n' \
    > /usr/local/bin/bothos-pi-adapter && chmod +x /usr/local/bin/bothos-pi-adapter
