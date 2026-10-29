# syntax=docker/dockerfile:1

# Build the Linux executable from the monitor's Go source.
FROM golang:1-alpine AS builder

WORKDIR /src

# Cache downloaded modules until go.mod or go.sum changes.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN CGO_ENABLED=0 go build \
    -mod=readonly \
    -trimpath \
    -buildvcs=false \
    -ldflags="-s -w" \
    -o /out/ref-ledger-monitor \
    ./cmd/server

# Runtime: Go compiler and source are not included.
FROM alpine:3 AS runtime

# MongoDB Atlas requires trusted CA certificates for TLS.
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 monitor \
    && adduser -S -D -H -u 10001 -G monitor monitor

WORKDIR /app

COPY --from=builder /out/ref-ledger-monitor /app/ref-ledger-monitor
COPY web/ /app/web/

# Listen on all container interfaces, not laptop-only 127.0.0.1.
# These defaults can be overridden in Back4App environment settings.
ENV APP_ENV=production \
    BIND_ADDRESS=0.0.0.0 \
    PORT=8080 \
    WEB_DIR=/app/web

USER monitor

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD wget -q -T 3 -O /dev/null "http://127.0.0.1:${PORT}/healthz" || exit 1

ENTRYPOINT ["/app/ref-ledger-monitor"]
