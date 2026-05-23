// P901 device simulator — acts as a long-running JT808 GPS tracker.
//
// It maintains persistent state (position, heading, battery, signal) in a JSON
// file so that restarting the container continues the device's journey rather
// than teleporting it back to the starting point.
//
// State file: /data/state.json  (mount a volume at /data to persist across restarts)
//
// Required env:
//
//	DEVICE_IMEI   12-digit device IMEI / phone number (e.g. "013987654321")
//
// Optional env:
//
//	SERVER_ADDR           JT808 server host:port            (default: jt808:7018)
//	LOCATION_INTERVAL     how often to send a location fix  (default: 30s)
//	HEARTBEAT_INTERVAL    how often to send a heartbeat     (default: 60s)
//	STATE_FILE            path to persist device state      (default: /data/state.json)
//	DEVICE_MODEL          model string in registration      (default: P901)
//	INITIAL_LAT           starting latitude  (used only when no state file exists)
//	INITIAL_LON           starting longitude
//	INITIAL_HEADING       starting heading in degrees       (default: 45)
//	INITIAL_SPEED         initial speed km/h                (default: 40)
//	INITIAL_BATTERY       initial battery %                 (default: 100)
//	APP_DEBUG             verbose logging                   (default: false)
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"jt808-server/pkg/protocol"
	"math"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// ── Device state (persisted to disk) ─────────────────────────────────────────

type State struct {
	Latitude  float64   `json:"latitude"`
	Longitude float64   `json:"longitude"`
	Heading   float64   `json:"heading"`    // degrees 0-359
	Speed     float64   `json:"speed"`      // km/h
	Battery   float64   `json:"battery"`    // 0-100
	Altitude  uint16    `json:"altitude"`   // metres
	SavedAt   time.Time `json:"saved_at"`
}

func defaultState(cfg *deviceConfig) State {
	return State{
		Latitude:  cfg.InitialLat,
		Longitude: cfg.InitialLon,
		Heading:   cfg.InitialHeading,
		Speed:     cfg.InitialSpeed,
		Battery:   cfg.InitialBattery,
		Altitude:  220,
		SavedAt:   time.Now(),
	}
}

func loadState(path string, cfg *deviceConfig) State {
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultState(cfg)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return defaultState(cfg)
	}
	return s
}

func saveState(s State, path string) {
	s.SavedAt = time.Now()
	data, _ := json.MarshalIndent(s, "", "  ")
	_ = os.MkdirAll(stateDir(path), 0755)
	_ = os.WriteFile(path, data, 0644)
}

func stateDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}

// ── Config ────────────────────────────────────────────────────────────────────

type deviceConfig struct {
	IMEI              string
	ServerAddr        string
	LocationInterval  time.Duration
	HeartbeatInterval time.Duration
	StateFile         string
	DeviceModel       string
	InitialLat        float64
	InitialLon        float64
	InitialHeading    float64
	InitialSpeed      float64
	InitialBattery    float64
	Debug             bool
}

func loadConfig() *deviceConfig {
	imei := os.Getenv("DEVICE_IMEI")
	if imei == "" {
		fmt.Fprintln(os.Stderr, "DEVICE_IMEI environment variable is required")
		os.Exit(1)
	}
	return &deviceConfig{
		IMEI:              imei,
		ServerAddr:        envStr("SERVER_ADDR", "jt808:7018"),
		LocationInterval:  envDuration("LOCATION_INTERVAL", 30*time.Second),
		HeartbeatInterval: envDuration("HEARTBEAT_INTERVAL", 60*time.Second),
		StateFile:         envStr("STATE_FILE", "/data/state.json"),
		DeviceModel:       envStr("DEVICE_MODEL", "P901"),
		InitialLat:        envFloat("INITIAL_LAT", 31.5204),
		InitialLon:        envFloat("INITIAL_LON", 74.3587),
		InitialHeading:    envFloat("INITIAL_HEADING", 45.0),
		InitialSpeed:      envFloat("INITIAL_SPEED", 40.0),
		InitialBattery:    envFloat("INITIAL_BATTERY", 100.0),
		Debug:             os.Getenv("APP_DEBUG") == "true",
	}
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	cfg := loadConfig()

	var log *zap.Logger
	if cfg.Debug {
		log, _ = zap.NewDevelopment()
	} else {
		log, _ = zap.NewProduction()
	}
	defer log.Sync() //nolint:errcheck

	state := loadState(cfg.StateFile, cfg)
	log.Info("P901 device simulator starting",
		zap.String("imei", cfg.IMEI),
		zap.String("server", cfg.ServerAddr),
		zap.Float64("lat", state.Latitude),
		zap.Float64("lon", state.Longitude),
		zap.Float64("battery", state.Battery),
	)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		for {
			if err := runDevice(cfg, &state, log); err != nil {
				log.Warn("device disconnected — reconnecting in 5s", zap.Error(err))
				saveState(state, cfg.StateFile)
			}
			select {
			case <-quit:
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()

	<-quit
	log.Info("shutdown — saving state")
	saveState(state, cfg.StateFile)
}

// ── Device session ────────────────────────────────────────────────────────────

func runDevice(cfg *deviceConfig, state *State, log *zap.Logger) error {
	log.Info("connecting", zap.String("addr", cfg.ServerAddr))
	conn, err := net.DialTimeout("tcp", cfg.ServerAddr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	r := bufio.NewReaderSize(conn, 4096)
	seq := uint16(0)
	next := func() uint16 { seq++; return seq }

	// ── Register ──────────────────────────────────────────────────────────────
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := protocol.WriteFrame(conn, protocol.MsgRegistration, cfg.IMEI, next(), buildRegBody(cfg)); err != nil {
		return fmt.Errorf("send registration: %w", err)
	}
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	regResp, err := protocol.ReadFrame(r)
	if err != nil {
		return fmt.Errorf("read reg response: %w", err)
	}
	if regResp.MsgID != protocol.MsgRegistrationResp || len(regResp.Body) < 3 {
		return fmt.Errorf("unexpected reg response 0x%04X", regResp.MsgID)
	}
	if regResp.Body[2] != protocol.RegSuccess {
		return fmt.Errorf("registration rejected: result=0x%02X (device not approved)", regResp.Body[2])
	}
	token := string(regResp.Body[3:])
	log.Info("registered", zap.String("imei", cfg.IMEI), zap.String("token", token))

	// ── Authenticate ──────────────────────────────────────────────────────────
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := protocol.WriteFrame(conn, protocol.MsgAuthentication, cfg.IMEI, next(), []byte(token)); err != nil {
		return fmt.Errorf("send auth: %w", err)
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	authACK, err := protocol.ReadFrame(r)
	if err != nil {
		return fmt.Errorf("read auth ACK: %w", err)
	}
	if authACK.MsgID != protocol.MsgPlatformResp || len(authACK.Body) < 5 || authACK.Body[4] != protocol.ResultOK {
		return fmt.Errorf("authentication failed")
	}
	log.Info("authenticated", zap.String("imei", cfg.IMEI))

	// ── Main loop ─────────────────────────────────────────────────────────────
	locTick := time.NewTicker(cfg.LocationInterval)
	hbTick := time.NewTicker(cfg.HeartbeatInterval)
	defer locTick.Stop()
	defer hbTick.Stop()

	// Send first location immediately on connect.
	if err := sendLocation(conn, cfg.IMEI, next(), state, log); err != nil {
		return err
	}
	if err := readACK(conn, r, protocol.MsgLocationReport); err != nil {
		return err
	}
	advanceState(state, cfg)
	saveState(*state, cfg.StateFile)

	for {
		select {
		case <-locTick.C:
			if err := sendLocation(conn, cfg.IMEI, next(), state, log); err != nil {
				return err
			}
			if err := readACK(conn, r, protocol.MsgLocationReport); err != nil {
				return err
			}
			advanceState(state, cfg)
			saveState(*state, cfg.StateFile)

		case <-hbTick.C:
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := protocol.WriteFrame(conn, protocol.MsgHeartbeat, cfg.IMEI, next(), nil); err != nil {
				return fmt.Errorf("send heartbeat: %w", err)
			}
			conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			if err := readACK(conn, r, protocol.MsgHeartbeat); err != nil {
				return err
			}
			log.Debug("heartbeat ACK", zap.String("imei", cfg.IMEI))
		}
	}
}

// ── Frame helpers ─────────────────────────────────────────────────────────────

func sendLocation(conn net.Conn, imei string, seq uint16, state *State, log *zap.Logger) error {
	body := buildLocationBody(state)
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := protocol.WriteFrame(conn, protocol.MsgLocationReport, imei, seq, body); err != nil {
		return fmt.Errorf("send location: %w", err)
	}
	log.Info("location sent",
		zap.String("imei", imei),
		zap.Float64("lat", state.Latitude),
		zap.Float64("lon", state.Longitude),
		zap.Float64("speed", state.Speed),
		zap.Float64("heading", state.Heading),
		zap.Int("battery", int(state.Battery)),
	)
	return nil
}

func readACK(conn net.Conn, r *bufio.Reader, forMsg uint16) error {
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetReadDeadline(time.Time{}) // clear so the next idle period doesn't expire
	f, err := protocol.ReadFrame(r)
	if err != nil {
		return fmt.Errorf("read ACK: %w", err)
	}
	if f.MsgID != protocol.MsgPlatformResp {
		return fmt.Errorf("expected 0x8001, got 0x%04X", f.MsgID)
	}
	if len(f.Body) >= 5 {
		replyMsgID := binary.BigEndian.Uint16(f.Body[2:4])
		result := f.Body[4]
		if replyMsgID != forMsg {
			return fmt.Errorf("ACK for wrong msg: expected 0x%04X got 0x%04X", forMsg, replyMsgID)
		}
		if result != protocol.ResultOK {
			return fmt.Errorf("server returned result 0x%02X for 0x%04X", result, forMsg)
		}
	}
	return nil
}

// ── Body builders ─────────────────────────────────────────────────────────────

func buildRegBody(cfg *deviceConfig) []byte {
	b := make([]byte, 0, 44)
	b = appendU16(b, 6)  // province: Punjab
	b = appendU16(b, 1)  // city: Lahore
	b = append(b, padRight("SIMCO", 5)...)
	b = append(b, padRight(cfg.DeviceModel, 20)...)
	devID := cfg.IMEI
	if len(devID) > 7 {
		devID = devID[len(devID)-7:]
	}
	b = append(b, padRight(devID, 7)...)
	b = append(b, 0x02) // plate colour: blue
	b = append(b, []byte("P901")...)
	return b
}

func buildLocationBody(s *State) []byte {
	b := make([]byte, 0, 36)

	statusFlags := protocol.StatusLocated | protocol.StatusACCOn
	lat, lon := s.Latitude, s.Longitude
	if lat < 0 {
		statusFlags |= protocol.StatusLatSouth
		lat = -lat
	}
	if lon < 0 {
		statusFlags |= protocol.StatusLonWest
		lon = -lon
	}

	b = appendU32(b, 0) // no alarm
	b = appendU32(b, statusFlags)
	b = appendU32(b, uint32(lat*1e6))
	b = appendU32(b, uint32(lon*1e6))
	b = appendU16(b, s.Altitude)
	b = appendU16(b, uint16(s.Speed*10)) // 0.1 km/h units
	b = appendU16(b, uint16(s.Heading))
	b = append(b, bcdTimestamp(time.Now().UTC())...)

	// TLV: battery level (0xEB), signal strength (0x30)
	battPct := byte(clamp(int(s.Battery), 0, 100))
	b = append(b, 0xEB, 0x01, battPct)

	csq := byte(simSignal())
	b = append(b, 0x30, 0x01, csq)

	return b
}

// ── State advancement ─────────────────────────────────────────────────────────

// advanceState moves the device one step forward in time.
// Position is calculated from heading + speed + elapsed interval.
// Heading drifts randomly to simulate road curvature.
// Battery drains slightly each step.
func advanceState(s *State, cfg *deviceConfig) {
	dt := cfg.LocationInterval.Seconds() / 3600.0 // hours

	// Drift heading ±12° per step (smooth road-like curve)
	s.Heading += (rand.Float64()*24 - 12)
	s.Heading = math.Mod(s.Heading+360, 360)

	// Vary speed ±5 km/h per step
	s.Speed += (rand.Float64()*10 - 5)
	s.Speed = clampF(s.Speed, 10, 90)

	// Move along heading
	headingRad := s.Heading * math.Pi / 180
	distKm := s.Speed * dt

	// 1 degree latitude ≈ 111 km; longitude varies by cos(lat)
	s.Latitude += distKm * math.Cos(headingRad) / 111.0
	s.Longitude += distKm * math.Sin(headingRad) / (111.0 * math.Cos(s.Latitude*math.Pi/180))

	// Altitude varies slightly
	s.Altitude = uint16(clamp(int(s.Altitude)+rand.Intn(5)-2, 180, 300))

	// Battery drains ~0.05% per location report (full charge lasts ~33 hours at 30s interval)
	s.Battery -= 0.05
	if s.Battery < 5 {
		s.Battery = 5 // device keeps transmitting on reserve
	}
}

func simSignal() int {
	// CSQ range 10-28, with occasional weak signal
	base := 18.0
	noise := rand.NormFloat64() * 4
	csq := int(base + noise)
	return clamp(csq, 5, 28)
}

// ── Encoding helpers ──────────────────────────────────────────────────────────

func bcdTimestamp(t time.Time) []byte {
	bcd := func(n int) byte { return byte((n/10)<<4 | (n % 10)) }
	return []byte{
		bcd(t.Year() % 100),
		bcd(int(t.Month())),
		bcd(t.Day()),
		bcd(t.Hour()),
		bcd(t.Minute()),
		bcd(t.Second()),
	}
}

func appendU16(b []byte, v uint16) []byte { return append(b, byte(v>>8), byte(v)) }
func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func padRight(s string, n int) []byte {
	b := make([]byte, n)
	copy(b, s)
	return b
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ── Env helpers ───────────────────────────────────────────────────────────────

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
