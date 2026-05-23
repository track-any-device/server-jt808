# Linux Kernel Tuning for High-Connection JT808 Servers

Apply these on every Swarm node that runs the JT808 container.
Add to `/etc/sysctl.d/99-jt808.conf` then run `sysctl --system`.

## File Descriptor Limits

Each TCP connection = 1 file descriptor. Default limit is 1024 per process.

```bash
# /etc/security/limits.d/jt808.conf
# (or set in Docker: ulimits: nofile: soft: 100000 hard: 100000)
*    soft nofile 100000
*    hard nofile 100000
root soft nofile 100000
root hard nofile 100000
```

## TCP Socket Tuning

```ini
# /etc/sysctl.d/99-jt808.conf

# ── Port range — needed if the server also makes outbound connections ────────
net.ipv4.ip_local_port_range = 10000 65535

# ── Connection backlog ───────────────────────────────────────────────────────
# somaxconn caps the listen() backlog (accept queue depth).
# If bursts of reconnects arrive faster than accept() processes them,
# increase this. 10k is safe for 100k device systems.
net.core.somaxconn = 10240
net.ipv4.tcp_max_syn_backlog = 10240

# ── Keepalive — detect dead peers faster than the application timeout ────────
# These are OS-level keepalives (independent of our Go HeartbeatTimeout).
net.ipv4.tcp_keepalive_time = 60       # start probing after 60s idle
net.ipv4.tcp_keepalive_intvl = 10      # probe every 10s
net.ipv4.tcp_keepalive_probes = 6      # 6 failed probes = dead

# ── TIME_WAIT recycling ──────────────────────────────────────────────────────
# JT808 devices reconnect often. Without this, TIME_WAIT sockets accumulate.
net.ipv4.tcp_fin_timeout = 15
net.ipv4.tcp_tw_reuse = 1

# ── Socket buffers — tune for throughput at scale ────────────────────────────
# Each JT808 location packet is ~100 bytes. These defaults are fine for 10k.
# For 100k+ or batch location uploads, increase:
net.core.rmem_max = 16777216           # 16 MB receive buffer max
net.core.wmem_max = 16777216           # 16 MB send buffer max
net.ipv4.tcp_rmem = 4096 87380 16777216
net.ipv4.tcp_wmem = 4096 65536 16777216

# ── Connection tracking ──────────────────────────────────────────────────────
# nf_conntrack limits total tracked connections. Default is often 65536.
# For 100k devices, set this to at least 200k.
# (Only needed if iptables/nftables conntrack is active — Docker Swarm uses it)
net.netfilter.nf_conntrack_max = 262144
net.netfilter.nf_conntrack_tcp_timeout_established = 3600
```

## Memory Estimation

| Connections | Go goroutines | Approximate RAM |
|---|---|---|
| 1,000 | 1,000 | ~50 MB |
| 10,000 | 10,000 | ~200 MB |
| 50,000 | 50,000 | ~800 MB |
| 100,000 | 100,000 | ~1.5 GB |

Go goroutines start at 8 KB stack. At 100k connections that's 800 MB stack alone.
At this scale, run multiple nodes (global mode) rather than one fat node.

## Scale Guide

| Device count | Nodes | Replicas | Redis pool/replica | Recommended instance |
|---|---|---|---|---|
| < 5,000 | 1 | 1 | 50 | 2 CPU / 1 GB |
| 10,000 | 2 | 2 | 100 | 4 CPU / 2 GB |
| 50,000 | 5 | 5 | 150 | 8 CPU / 4 GB |
| 100,000 | 10 | 10 | 200 | 16 CPU / 8 GB |

Redis should be clustered at 50k+ devices to handle the XADD throughput.
InfluxDB should replace MySQL for location history at this scale.
