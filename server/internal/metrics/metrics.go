// Package metrics exposes Prometheus instrumentation for the JT808 server.
//
// Scrape endpoint: GET /metrics (HTTP server on JT808_HTTP_ADDR, default :9090)
//
// Key metrics to alert on:
//   - jt808_connections_active > 0 always (otherwise all devices are offline)
//   - jt808_auth_violations_total rate > 0 (possible spoofing attempt)
//   - jt808_decode_errors_total rate > 0 (firmware version mismatch or corruption)
//   - jt808_sos_alarms_total rate (critical — page on any increment)
//   - jt808_stream_publish_lag_seconds p99 > 1 (Redis backpressure)
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics groups all Prometheus instruments.
type Metrics struct {
	// Connection gauges
	ConnectionsActive prometheus.Gauge
	ConnectionsTotal  prometheus.Counter

	// Per message-type counter
	FramesReceived *prometheus.CounterVec

	// Auth
	AuthSuccesses  prometheus.Counter
	AuthViolations prometheus.Counter

	// Protocol health
	Heartbeats      prometheus.Counter
	LocationReports prometheus.Counter
	SOSAlarms       prometheus.Counter
	DecodeErrors    prometheus.Counter
	UnknownMessages prometheus.Counter

	// Forwarding latency
	StreamPublishDuration prometheus.Histogram
}

// New registers all metrics with the default Prometheus registry.
func New() *Metrics {
	return &Metrics{
		ConnectionsActive: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "jt808_connections_active",
			Help: "Current number of active device TCP connections on this replica.",
		}),
		ConnectionsTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "jt808_connections_total",
			Help: "Total TCP connections accepted since startup.",
		}),
		FramesReceived: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "jt808_frames_received_total",
			Help: "Total JT808 frames received, by message type.",
		}, []string{"msg_type"}),
		AuthSuccesses: promauto.NewCounter(prometheus.CounterOpts{
			Name: "jt808_auth_successes_total",
			Help: "Successful device authentications.",
		}),
		AuthViolations: promauto.NewCounter(prometheus.CounterOpts{
			Name: "jt808_auth_violations_total",
			Help: "Authentication failures (bad token, unauthenticated message).",
		}),
		Heartbeats: promauto.NewCounter(prometheus.CounterOpts{
			Name: "jt808_heartbeats_total",
			Help: "Total heartbeat frames received.",
		}),
		LocationReports: promauto.NewCounter(prometheus.CounterOpts{
			Name: "jt808_location_reports_total",
			Help: "Total location reports received (0x0200 + 0x0704 items).",
		}),
		SOSAlarms: promauto.NewCounter(prometheus.CounterOpts{
			Name: "jt808_sos_alarms_total",
			Help: "Total SOS alarm events received.",
		}),
		DecodeErrors: promauto.NewCounter(prometheus.CounterOpts{
			Name: "jt808_decode_errors_total",
			Help: "Frame decode failures (checksum, short body, TLV parse).",
		}),
		UnknownMessages: promauto.NewCounter(prometheus.CounterOpts{
			Name: "jt808_unknown_messages_total",
			Help: "Frames with unrecognised message IDs.",
		}),
		StreamPublishDuration: promauto.NewHistogram(prometheus.HistogramOpts{
			Name:    "jt808_stream_publish_seconds",
			Help:    "Redis Stream XADD latency in seconds.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1.0},
		}),
	}
}
