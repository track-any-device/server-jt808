package protocol

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"
)

// DecodeLocation parses a 0x0200 location report body.
// The spec mandates a 28-byte fixed header followed by variable-length TLV items.
func DecodeLocation(body []byte) (*LocationReport, error) {
	if len(body) < 28 {
		return nil, fmt.Errorf("location body too short: %d bytes (need ≥28)", len(body))
	}

	r := bytes.NewReader(body)
	loc := &LocationReport{}

	binary.Read(r, binary.BigEndian, &loc.AlarmFlags)  //nolint:errcheck
	binary.Read(r, binary.BigEndian, &loc.StatusFlags) //nolint:errcheck

	var rawLat, rawLon uint32
	binary.Read(r, binary.BigEndian, &rawLat) //nolint:errcheck
	binary.Read(r, binary.BigEndian, &rawLon) //nolint:errcheck

	// Raw values are in units of 1×10⁻⁶ degrees (micro-degrees).
	lat := float64(rawLat) / 1e6
	lon := float64(rawLon) / 1e6
	if loc.StatusFlags&StatusLatSouth != 0 {
		lat = -lat
	}
	if loc.StatusFlags&StatusLonWest != 0 {
		lon = -lon
	}
	loc.Latitude = lat
	loc.Longitude = lon

	var altRaw, speedRaw, dirRaw uint16
	binary.Read(r, binary.BigEndian, &altRaw)   //nolint:errcheck
	binary.Read(r, binary.BigEndian, &speedRaw) //nolint:errcheck
	binary.Read(r, binary.BigEndian, &dirRaw)   //nolint:errcheck
	loc.Altitude = altRaw
	loc.Speed = float64(speedRaw) / 10.0 // wire unit is 0.1 km/h
	loc.Direction = dirRaw

	// 6-byte BCD timestamp: YYMMDDHHmmss (device local time; usually UTC+8 for China).
	// We store it as-is with a Z suffix — callers must apply timezone if needed.
	tsBCD := make([]byte, 6)
	r.Read(tsBCD) //nolint:errcheck
	loc.Timestamp = parseBCDTimestamp(tsBCD)

	// Convenience booleans
	loc.GPSFixed = loc.StatusFlags&StatusLocated != 0
	loc.ACCOn = loc.StatusFlags&StatusACCOn != 0

	// Parse variable-length additional info items (TLV format):
	// id(1) | length(1) | value(length)
	loc.Extras = make(map[uint8][]byte)
	for r.Len() >= 2 {
		var id, length uint8
		binary.Read(r, binary.BigEndian, &id)     //nolint:errcheck
		binary.Read(r, binary.BigEndian, &length) //nolint:errcheck
		if r.Len() < int(length) {
			break // malformed TLV trailer — stop gracefully
		}
		val := make([]byte, length)
		r.Read(val) //nolint:errcheck
		loc.Extras[id] = val
	}

	return loc, nil
}

// DecodeRegistration parses a 0x0100 terminal registration body.
//
// Real-world JT808 devices ship two common registration body layouts:
//
//	Standard (JT808-2011/2013/2019): province(2)+city(2)+manuf(5)+model(20)+devid(7)+platecolor(1)+plate(N) = 37+N bytes
//	Compact  (many cheap trackers):  province(2)+city(2)+manuf(5)+model(8) +devid(7)+platecolor(1)+plate(N) = 25+N bytes
//
// We detect which layout the device uses from the total body length:
//
//	after province+city+manuf (9 bytes consumed), remaining = body - 9
//	  remaining >= 28 → model field is 20 bytes (standard)
//	  remaining < 28  → model field is 8 bytes  (compact)
func DecodeRegistration(body []byte) (*RegistrationInfo, error) {
	// Minimum: province(2) + city(2) + manuf(5) + devid(7) + platecolor(1) = 17 bytes.
	// Model field and plate number may be absent on severely stripped firmware.
	if len(body) < 17 {
		return nil, fmt.Errorf("registration body too short: %d bytes (need ≥17)", len(body))
	}
	r := bytes.NewReader(body)
	info := &RegistrationInfo{}

	binary.Read(r, binary.BigEndian, &info.ProvinceID) //nolint:errcheck
	binary.Read(r, binary.BigEndian, &info.CityID)     //nolint:errcheck

	manufBytes := make([]byte, 5)
	r.Read(manufBytes) //nolint:errcheck
	info.ManufID = nullTrimmed(manufBytes)

	// Detect model field length: standard=20, compact=8.
	// After province+city+manuf we've consumed 9 bytes.
	// Remaining must cover: model + devid(7) + platecolor(1) + plate(≥0).
	// If remaining ≥ 28 the standard 20-byte model fits; otherwise use 8.
	modelLen := 8
	if r.Len() >= 28 {
		modelLen = 20
	}
	if r.Len() >= modelLen {
		modelBytes := make([]byte, modelLen)
		r.Read(modelBytes) //nolint:errcheck
		info.DeviceModel = nullTrimmed(modelBytes)
	}

	if r.Len() >= 7 {
		devIDBytes := make([]byte, 7)
		r.Read(devIDBytes) //nolint:errcheck
		info.DeviceID = nullTrimmed(devIDBytes)
	}

	if r.Len() >= 1 {
		binary.Read(r, binary.BigEndian, &info.PlateColor) //nolint:errcheck
	}

	if r.Len() > 0 {
		plateBytes := make([]byte, r.Len())
		r.Read(plateBytes) //nolint:errcheck
		info.PlateNumber = nullTrimmed(plateBytes)
	}

	return info, nil
}

// parseBCDTimestamp converts 6 BCD bytes (YYMMDDHHmmss) to an ISO-8601 string.
// Year is assumed to be 20YY.
func parseBCDTimestamp(b []byte) string {
	d := make([]byte, 12)
	for i, v := range b {
		d[i*2] = '0' + (v>>4)&0x0f
		d[i*2+1] = '0' + v&0x0f
	}
	// Format: "20YY-MM-DDTHH:mm:ssZ"
	return fmt.Sprintf("20%s-%s-%sT%s:%s:%sZ",
		d[0:2], d[2:4], d[4:6], d[6:8], d[8:10], d[10:12])
}

// GenerateAuthToken creates a simple time-based auth token for device registration.
// In production, store and verify this in Redis (see session.Registry).
func GenerateAuthToken(phone string) string {
	// Simple: phone + Unix-second truncated to 10 digits.
	// Replace with a cryptographically random token if tighter security is needed.
	ts := time.Now().Unix()
	return fmt.Sprintf("%s_%d", phone, ts)
}

func nullTrimmed(b []byte) string {
	return string(bytes.TrimRight(b, "\x00 "))
}
