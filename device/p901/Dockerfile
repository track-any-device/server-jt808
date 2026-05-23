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
    -o /p901 \
    ./device/p901

# ── Runtime ───────────────────────────────────────────────────────────────────
FROM alpine:3.20

RUN apk add --no-cache ca-certificates \
    && addgroup -S p901 \
    && adduser -S -G p901 p901 \
    && mkdir -p /data && chown p901:p901 /data

COPY --from=builder /p901 /p901

# /data holds state.json — mount a named volume to persist across restarts
VOLUME ["/data"]

USER p901
ENTRYPOINT ["/p901"]
