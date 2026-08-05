# Multi-stage build for the bothos spine containers.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# Deterministic scanners, built for the runtime stage. osv-scanner and
# govulncheck are Go-installable; they run against a shallow clone inside
# the worker/scan container.
RUN go install golang.org/x/vuln/cmd/govulncheck@latest \
 && go install github.com/google/osv-scanner/v2/cmd/osv-scanner@latest
COPY . .
RUN CGO_ENABLED=0 go build -o /out/gateway ./cmd/gateway \
 && CGO_ENABLED=0 go build -o /out/worker ./cmd/worker \
 && CGO_ENABLED=0 go build -o /out/scan ./cmd/scan

FROM alpine:3.20
RUN apk add --no-cache ca-certificates git
COPY --from=build /go/bin/govulncheck /usr/local/bin/govulncheck
COPY --from=build /go/bin/osv-scanner /usr/local/bin/osv-scanner
COPY --from=build /out/gateway /usr/local/bin/bothos-gateway
COPY --from=build /out/worker /usr/local/bin/bothos-worker
COPY --from=build /out/scan /usr/local/bin/bothos-scan
