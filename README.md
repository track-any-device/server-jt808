# JT808 GPS Server

A high-performance Go server that speaks the **JT/T 808-2013/2019** protocol used by Chinese GPS tracking devices.  
Devices connect over a persistent TCP socket; the server decodes frames, validates sessions, and forwards telemetry to Laravel via a **Redis Stream**.

<!-- VERSIONS_START -->
| Image | Latest | Pull |
|-------|--------|------|
| `jt808-server` | `v0.1.0` | `docker pull $DOCKERHUB_USERNAME/jt808-server:0.1.0` |
| `p901-device`  | `v0.1.0` | `docker pull $DOCKERHUB_USERNAME/p901-device:0.1.0`  |
<!-- VERSIONS_END -->

---

## Architecture

```
GPS Devices (JT808 TCP)
       │
       ▼  :7018
┌─────────────────────┐
│   JT808 Go Server   │
│                     │
│  per-device goroutine  ─── decode frame
│  session registry   │      │
│  auth flow          │      ▼
│  location handler   │  Redis Stream (jt808:telemetry)
│                     │      │
│  :9090 HTTP         │      ▼
│  /healthz /metrics  │  Laravel Queue Worker
└─────────────────────┘      │
       │                     ▼
       └─── MySQL  MySQL (device approval, device_locations)
```

### Device lifecycle

| Step | Device → Server | Server → Device |
|------|-----------------|-----------------|
| 1 | TCP connect | — |
| 2 | `0x0100` Registration | `0x8100` with auth token |
| 3 | `0x0102` Authentication | `0x8001` ACK |
| 4 | `0x0002` Heartbeat (periodic) | `0x8001` ACK |
| 5 | `0x0200` Location report | `0x8001` ACK |
| 6 | TCP disconnect / heartbeat timeout | session cleaned up |

Unapproved or unknown devices are rejected at step 2 and auto-created in the MySQL `devices` table as `pending` so an admin can approve them in the Filament dashboard.

---

## Ports

| Port | Protocol | Purpose |
|------|----------|---------|
| `7018` | TCP | JT808 binary framing — GPS devices connect here |
| `9090` | HTTP | Observability: `/healthz`, `/readyz`, `/metrics` |

---

## Quick start (Docker Compose)

```bash
# 1. Copy and edit the env file
cp .env.example .env

# 2. Build and start everything (jt808 + Redis + MySQL)
docker compose up --build

# 3. Tail logs
docker compose logs -f jt808

# 4. Check health
curl http://localhost:9090/healthz   # → ok
curl http://localhost:9090/readyz    # → ready
curl http://localhost:9090/metrics   # → Prometheus text

# 5. Stop and wipe volumes
docker compose down -v
```

The server retries until both Redis and MySQL are reachable before entering the accept loop — no manual startup ordering needed.

---

## Environment variables

All variables have safe defaults; only `DB_PASSWORD` and optionally `REDIS_PASSWORD` need to be set in production.

### TCP / HTTP

| Variable | Default | Description |
|----------|---------|-------------|
| `JT808_TCP_ADDR` | `:7018` | TCP listen address for GPS devices |
| `JT808_HTTP_ADDR` | `:9090` | HTTP listen address for observability |

### Redis

| Variable | Default | Description |
|----------|---------|-------------|
| `REDIS_HOST` | `redis` | Redis hostname |
| `REDIS_PORT` | `6379` | Redis port |
| `REDIS_PASSWORD` | _(empty)_ | Redis password; leave empty to disable auth |
| `REDIS_JT808_DB` | `0` | Redis logical DB index (0–15) |
| `REDIS_POOL_SIZE` | `100` | Max connections in the Redis pool |

### Redis key layout

| Variable | Default | Description |
|----------|---------|-------------|
| `STREAM_KEY` | `jt808:telemetry` | Redis Stream key — Laravel workers consume from here |
| `STREAM_MAX_LEN` | `100000` | Approximate cap on stream length (`MAXLEN ~`) |
| `SESSION_PREFIX` | `jt808:session:` | Hash key prefix for per-device session state |
| `AUTH_TOKEN_PREFIX` | `jt808:authtoken:` | String key prefix for one-time auth tokens |
| `ONLINE_Z_KEY` | `jt808:online` | Sorted-set tracking last heartbeat timestamps |
| `CMD_CHANNEL` | `jt808:cmd:` | Pub/sub channel prefix for server→device commands |

> Multiple environments (dev / staging / prod) can share one Redis instance by changing these prefixes.

### MySQL

| Variable | Default | Description |
|----------|---------|-------------|
| `DB_HOST` | `mysql` | MySQL hostname |
| `DB_PORT` | `3306` | MySQL port |
| `DB_USERNAME` | `laravel` | MySQL user |
| `DB_PASSWORD` | _(empty)_ | MySQL password |
| `DB_DATABASE` | `laravel` | Database name |
| `DB_DEVICE_TYPE_ID` | `1` | `device_type_id` assigned when auto-creating unknown devices |

### Protocol timings

| Variable | Default | Description |
|----------|---------|-------------|
| `AUTH_TIMEOUT` | `30s` | Time allowed for a device to complete registration + authentication after connecting. Unauthenticated connections are closed after this window. |
| `HEARTBEAT_TIMEOUT` | `3m` | Idle timeout: a connection is dropped if no frame is received within this period. Devices typically send heartbeats every 60 s. |
| `WRITE_TIMEOUT` | `10s` | Per-write deadline for ACK / response frames; prevents slow-drain attacks. |

### Runtime

| Variable | Default | Description |
|----------|---------|-------------|
| `APP_DEBUG` | `false` | Set to `true` for human-readable development logs (zap development mode) |
| `SERVER_ID` | `<hostname>` | Unique identifier for this replica; tags session ownership in Redis. Auto-set to the container hostname in Docker. |

---

## Redis Stream format

Every message written to `STREAM_KEY` has an `event` field that determines its type.

### `event = location`

| Field | Type | Description |
|-------|------|-------------|
| `phone` | string | Device phone number / IMEI |
| `timestamp` | string | GPS fix time from the device (device clock) |
| `latitude` | float | Degrees, WGS-84 |
| `longitude` | float | Degrees, WGS-84 |
| `altitude` | int | Metres |
| `speed` | float | km/h |
| `direction` | int | Degrees from north (0–359) |
| `gps_fixed` | 0/1 | Whether the GPS has a fix |
| `acc_on` | 0/1 | ACC (ignition) status |
| `alarm_flags` | uint32 | Raw JT808 alarm bitmask |
| `active_alarms` | JSON array | Named alarms: `["sos","overspeed","low_battery","power_failure","vibration"]` |
| `battery_level` | int (optional) | Battery percentage 0–100 (from TLV if available) |
| `battery_from_flags` | int (optional) | Battery percentage estimated from status flags |
| `signal_strength` | int (optional) | GSM/LTE signal 0–100% (from TLV if available) |
| `extras` | JSON object (optional) | Raw TLV additional info: `{"0xID":"hex_bytes"}` |
| `published_at` | int64 | Unix milliseconds when the server wrote this entry |

### `event = device.registered`

| Field | Description |
|-------|-------------|
| `phone` | IMEI / phone number |
| `device_id` | Hardware device ID from registration frame |
| `device_model` | Model string from registration frame |
| `province_id`, `city_id` | Administrative codes |

### `event = device.authenticated`

| Field | Description |
|-------|-------------|
| `phone` | IMEI |
| `imei` | Hardware IMEI from registration |
| `addr` | Remote IP:port |
| `login_at` | RFC3339 timestamp |

---

## Prometheus metrics

Available at `http://localhost:9090/metrics`.

| Metric | Type | Description |
|--------|------|-------------|
| `jt808_connections_total` | Counter | Total TCP connections accepted |
| `jt808_connections_active` | Gauge | Currently open TCP connections |
| `jt808_frames_received_total` | Counter (by `msg_type`) | Frames received, labelled by message type |
| `jt808_location_reports_total` | Counter | Location frames successfully decoded |
| `jt808_heartbeats_total` | Counter | Heartbeat frames received |
| `jt808_auth_successes_total` | Counter | Successful authentications |
| `jt808_auth_violations_total` | Counter | Authentication failures (bad token) |
| `jt808_sos_alarms_total` | Counter | SOS alarm events |
| `jt808_decode_errors_total` | Counter | Frame decode / parse failures |
| `jt808_unknown_messages_total` | Counter | Unrecognised message IDs received |

---

## Docker Swarm (production)

See [stack.yml](stack.yml) for the Swarm deployment config.  
Key design decisions are documented in the comments at the top of that file, including why `mode: host` is required for JT808's long-lived TCP connections.

```bash
# Tag nodes that should run the JT808 server
docker node update --label-add jt808=true <node-id>

# Deploy
docker stack deploy -c stack.yml jt808
```

---

## Building manually

```bash
# Local binary
go build -o jt808-server ./cmd/server

# Docker image
docker build -t jt808-server:local .
```

---

## System tuning

For high connection counts (>10k devices) see [sysctl-tuning.md](sysctl-tuning.md) for recommended kernel parameter changes.
