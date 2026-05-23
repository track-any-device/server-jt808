# Build context: repo root (go.mod lives there)
FROM golang:1.23-alpine AS builder

WORKDIR /build

ENV GONOSUMDB=*

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /jt808-server \
    ./server/cmd

# ── Runtime ───────────────────────────────────────────────────────────────────
FROM alpine:3.20

RUN apk add --no-cache wget ca-certificates \
    && addgroup -S jt808 \
    && adduser -S -G jt808 jt808

COPY --from=builder /jt808-server /jt808-server

# 7018 — JT808 raw TCP
# 9090 — HTTP observability (/healthz, /readyz, /metrics)
EXPOSE 7018 9090

USER jt808
ENTRYPOINT ["/jt808-server"]
