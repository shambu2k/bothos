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
# static Go binaries and standalone scanners. Node 22 is required by the
# @earendil-works/pi-coding-agent SDK (>=22.19). A Go toolchain is NOT included;
# Go-ecosystem upgrades need it added later if used.
FROM node:22-alpine
# tini: the worker must run under a real init (PID 1) so it adopts orphans,
# forwards signals, and reaps zombies — the root cause of child processes
# (the PI RPC agent) dying mysteriously.
RUN apk add --no-cache ca-certificates git tini

# trivy is not in Alpine's community repo (v3.24), so download the release
# tarball. TARGETARCH is set by the Docker build engine; asset names are
# Linux-64bit (amd64) and Linux-ARM64 (arm64).
ARG TRIVY_VERSION=0.73.0
RUN ARCH=$(echo "$TARGETARCH" | sed 's/amd64/64bit/; s/arm64/ARM64/') && \
    wget -q "https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}/trivy_${TRIVY_VERSION}_Linux-${ARCH}.tar.gz" -O /tmp/trivy.tar.gz && \
    tar -xzf /tmp/trivy.tar.gz -C /usr/local/bin trivy && \
    rm /tmp/trivy.tar.gz

# PI sidecar adapter + its deps (installed before COPY admin so the layer caches).
COPY adapter/pi/package.json /opt/bothos/pi/package.json
RUN cd /opt/bothos/pi && npm install --no-audit --no-fund --omit=dev
COPY adapter/pi /opt/bothos/pi
# Expose the `pi` CLI (from @earendil-works/pi-coding-agent) so the worker can
# drive it via `--mode rpc`.
RUN ln -sf /opt/bothos/pi/node_modules/.bin/pi /usr/local/bin/pi

COPY --from=build /go/bin/govulncheck /usr/local/bin/govulncheck
COPY --from=build /go/bin/osv-scanner /usr/local/bin/osv-scanner
COPY --from=build /out/gateway /usr/local/bin/bothos-gateway
COPY --from=build /out/worker /usr/local/bin/bothos-worker
COPY --from=build /out/scan /usr/local/bin/bothos-scan
COPY --from=build /out/upgrade /usr/local/bin/bothos-upgrade

# Run the worker (and gateway) under tini so PID 1 is a proper init.
ENTRYPOINT ["tini", "--"]
