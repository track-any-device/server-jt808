# server-jt808 — AI Instructions

This is the **JT/T 808-2019 TCP server** for the Track Any Device platform.
Language: Go 1.23 | Docker image: `trackanydevice/server-jt808`

This server accepts long-lived TCP connections from JT808 GPS devices, decodes binary frames,
and publishes normalised telemetry to a Redis Stream consumed by the Laravel queue worker
(`package-jt808`'s `jt808:consume` command). Outbound commands arrive via Redis pub/sub.

Read this file before making any change.

---

## Platform-Wide Rules

These three rules apply in every repository under the `track-any-device` organisation.

**Cross-repo changes: file a GitHub issue first.**
If a task in this repository requires a change in another package or server app — stop. Open a
GitHub issue in the target repository describing exactly what is needed and why. Reference that
issue number in your commit message (`ref track-any-device/{repo}#{n}`). Do not directly edit
files in another repository.

The Redis Stream key (`jt808:telemetry`) and command channel pattern (`jt808:cmd:{phone}`) are
**shared contracts** with `package-jt808`. Any rename must be coordinated via a cross-repo issue
filed against `package-jt808` before merging here.

**Release order: packages before server apps.**
This is a Go server — it does not consume PHP packages. However, if a change here requires a
corresponding change in `package-jt808`, release `package-jt808` first (after `package-core`
if core also changed), then deploy this server.

**Database layer lives in `package-core` only.**
This server writes `device_locations` snapshots directly to MySQL for performance (Go cannot use
PHP migrations). Any schema change to `device_locations` must be initiated via an issue against
`package-core` to add the migration there first.

---

## Rule 1 — Plan before implementing

Before writing any code, ask clarifying questions. Present a plan and get explicit agreement.
Only begin once the approach is confirmed.

---

## Architecture

```
Device (TCP :7018)
  → per-device goroutine
  → frame decoder (binary JT808 framing: 0x7e marker, CRC16/CCITT)
  → Redis Stream XADD jt808:telemetry {msg_type, phone, imei, payload_json}

Outbound:
  Redis SUBSCRIBE jt808:cmd:{phone}
  → frame encoder → TCP socket write
```

One goroutine per connected device. Session state (phone, IMEI, auth token) is stored in
Redis under `jt808:session:{phone}` with a TTL of 3 minutes (reset on each heartbeat).

---

## Rule 2 — Never drop a device connection silently

When a device disconnects, the goroutine must:
1. Delete `jt808:session:{phone}` from Redis
2. Decrement the `jt808_connections_active` Prometheus gauge
3. Log the disconnect with the IMEI and reason

Silent goroutine exits cause phantom session entries and mislead the `offline` detection logic.

---

## Rule 3 — CRC must be validated on every inbound frame

All inbound frames must have their CRC16/CCITT checksum validated. Frames with invalid CRC
must be discarded and the `jt808_decode_errors_total` counter incremented. Never process a
corrupted frame.

---

## Prometheus Metrics (`:9090/metrics`)

| Metric | Type | Description |
|---|---|---|
| `jt808_connections_total` | Counter | Total TCP connections accepted |
| `jt808_connections_active` | Gauge | Currently connected devices |
| `jt808_frames_received_total` | Counter (by `msg_type`) | Frames decoded |
| `jt808_location_reports_total` | Counter | 0x0200 location frames processed |
| `jt808_heartbeats_total` | Counter | 0x0002 heartbeats received |
| `jt808_auth_success_total` | Counter | Successful 0x0102 auth |
| `jt808_auth_failure_total` | Counter | Failed auth attempts |
| `jt808_decode_errors_total` | Counter | Frames dropped due to CRC or parse error |
| `jt808_sos_alarms_total` | Counter | SOS alarm flags in location frames |

All new observable events must add a corresponding Prometheus counter or gauge.

---

## Device Lifecycle

```
1. TCP connect
2. 0x0100 Registration → server responds 0x8100 (auth token)
3. 0x0102 Authentication → server responds 0x8001 ACK
4. 0x0200 Location reports (ongoing)
5. 0x0002 Heartbeats (idle keep-alive)
6. Idle timeout (3 min) or TCP close → goroutine cleanup
```

Unapproved devices (not found in MySQL `devices` table): auto-insert as `status=pending`,
publish to Redis Stream with `approved=false` flag. `package-jt808` skips `SignalService`
for unapproved devices.

---

## Environment Variables

| Variable | Purpose |
|---|---|
| `TCP_ADDR` | TCP listen address (default `:7018`) |
| `METRICS_ADDR` | Prometheus metrics address (default `:9090`) |
| `REDIS_ADDR` | Redis connection string |
| `MYSQL_DSN` | MySQL DSN for device approval lookup |
| `SESSION_TTL_SECONDS` | Redis session TTL (default `180`) |

---

## Versioning

Docker images are published on every merge to `main`. Tags: `latest` + `vMAJOR.MINOR.PATCH`.
Versioning follows Go module conventions. Use `go.mod` for the canonical version.
