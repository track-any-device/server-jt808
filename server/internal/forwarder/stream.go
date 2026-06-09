// Package forwarder publishes decoded device telemetry to a Redis Stream.
//
// Laravel integration model:
//
//	JT808 Go Server
//	  └── XADD jt808:telemetry * event location phone lat lon ...
//	                │
//	                ▼ Redis Stream (persistent, replayable)
//	                │
//	  Laravel Queue Worker (reads via XREADGROUP)
//	    └── ProcessJt808Telemetry job
//	          ├── Upsert device_locations (MySQL / InfluxDB)
//	          ├── Evaluate geo-fence → create Incident if outside Beat
//	          └── Broadcast via Soketi (Laravel Echo → React map)
package forwarder

import (
	"context"
	"encoding/json"
	"fmt"
	"jt808-server/pkg/protocol"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Stream publishes telemetry to a Redis Stream.
type Stream struct {
	rdb       *redis.Client
	streamKey string
	maxLen    int64
	log       *zap.Logger
}

// NewStream creates a Stream publisher.
func NewStream(rdb *redis.Client, streamKey string, maxLen int64, log *zap.Logger) *Stream {
	return &Stream{rdb: rdb, streamKey: streamKey, maxLen: maxLen, log: log}
}

// PublishLocation encodes a location report and appends it to the stream.
func (s *Stream) PublishLocation(ctx context.Context, phone string, loc *protocol.LocationReport) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	values := map[string]any{
		"event":        "location",
		"phone":        phone,
		"timestamp":    loc.Timestamp,
		"altitude":     loc.Altitude,
		"speed":        loc.Speed,
		"direction":    loc.Direction,
		"gps_fixed":    boolInt(loc.GPSFixed),
		"acc_on":       boolInt(loc.ACCOn),
		"alarm_flags":  loc.AlarmFlags,
		"status_flags": loc.StatusFlags,
		"published_at": time.Now().UnixMilli(),
	}

	// Only include coordinates when the device has a valid GPS fix.
	// Without a fix the device sends 0,0 which would corrupt the last-known location snapshot.
	if loc.GPSFixed {
		values["latitude"] = loc.Latitude
		values["longitude"] = loc.Longitude
	}

	if battery := extractBattery(loc.Extras); battery >= 0 {
		values["battery_level"] = battery
	} else {
		battCode := (loc.StatusFlags >> 16) & 0x0F
		if battCode > 0 {
			values["battery_from_flags"] = battCode * 25
		}
	}
	if signal := extractSignal(loc.Extras); signal >= 0 {
		values["signal_strength"] = signal
	}

	if alarms := activeAlarmNames(loc.AlarmFlags); len(alarms) > 0 {
		alarmsJSON, _ := json.Marshal(alarms)
		values["active_alarms"] = string(alarmsJSON)
	} else {
		values["active_alarms"] = "[]"
	}

	if len(loc.Extras) > 0 {
		values["extras"] = extrasToJSON(loc.Extras)
	}

	fields := redis.XAddArgs{
		Stream: s.streamKey,
		MaxLen: s.maxLen,
		Approx: true,
		Values: values,
	}

	if id, err := s.rdb.XAdd(ctx, &fields).Result(); err != nil {
		s.log.Warn("stream publish failed", zap.String("phone", phone), zap.Error(err))
	} else {
		s.log.Info("location published",
			zap.String("phone", phone),
			zap.String("stream_id", id),
			zap.Bool("gps_fixed", loc.GPSFixed),
			zap.Float64("latitude", loc.Latitude),
			zap.Float64("longitude", loc.Longitude),
			zap.Float64("speed_kmh", loc.Speed),
		)
	}
}

// PublishEvent publishes a non-location event (device registered, SOS, offline, etc.)
func (s *Stream) PublishEvent(ctx context.Context, event, phone string, payload map[string]any) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	payloadJSON, _ := json.Marshal(payload)

	args := &redis.XAddArgs{
		Stream: s.streamKey,
		MaxLen: s.maxLen,
		Approx: true,
		Values: map[string]any{
			"event":        event,
			"phone":        phone,
			"payload":      string(payloadJSON),
			"published_at": time.Now().UnixMilli(),
		},
	}

	if _, err := s.rdb.XAdd(ctx, args).Result(); err != nil {
		s.log.Warn("event publish failed", zap.String("event", event), zap.String("phone", phone), zap.Error(err))
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func extractBattery(extras map[uint8][]byte) int {
	for _, id := range []uint8{0xEB, 0x63, 0x64} {
		if raw, ok := extras[id]; ok && len(raw) >= 1 {
			pct := int(raw[0])
			if pct >= 0 && pct <= 100 {
				return pct
			}
		}
	}
	if raw, ok := extras[0x25]; ok && len(raw) >= 2 {
		mV := int(raw[0])<<8 | int(raw[1])
		pct := (mV - 3400) * 100 / 800
		if pct < 0 {
			pct = 0
		} else if pct > 100 {
			pct = 100
		}
		return pct
	}
	return -1
}

func extractSignal(extras map[uint8][]byte) int {
	if raw, ok := extras[0x30]; ok && len(raw) >= 1 {
		csq := int(raw[0])
		if csq > 31 {
			csq = 31
		}
		return csq * 100 / 31
	}
	return -1
}

func activeAlarmNames(flags uint32) []string {
	type mapping struct {
		bit  uint32
		name string
	}
	mappings := []mapping{
		{protocol.AlarmSOS, "sos"},
		{protocol.AlarmOverspeed, "overspeed"},
		{protocol.AlarmLowBattery, "low_battery"},
		{protocol.AlarmPowerOff, "power_failure"},
		{protocol.AlarmVibration, "vibration"},
	}
	var active []string
	for _, m := range mappings {
		if flags&m.bit != 0 {
			active = append(active, m.name)
		}
	}
	return active
}

func extrasToJSON(extras map[uint8][]byte) string {
	parts := make([]string, 0, len(extras))
	for id, raw := range extras {
		parts = append(parts, fmt.Sprintf(`"0x%02X":"%X"`, id, raw))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
