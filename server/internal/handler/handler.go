// Package handler dispatches JT808 frames to the correct message handler.
//
// Message flow for a typical device lifecycle:
//
//  1. Device connects → TCP session created (StateConnected)
//  2. Device sends 0x0100 (registration) → server sends 0x8100 with auth token
//  3. Device sends 0x0102 (authentication) → server sends 0x8001 ACK → StateAuthenticated
//  4. Device sends 0x0002 (heartbeat) every N seconds → server ACKs
//  5. Device sends 0x0200 (location) → server parses, publishes to Redis Stream, ACKs
//  6. Device disconnects / timeout → session cleaned up
//
// When DeviceStore is nil (DB_ENABLED=false), all registrations are auto-approved.
package handler

import (
	"context"
	"fmt"
	"jt808-server/server/internal/config"
	"jt808-server/server/internal/forwarder"
	"jt808-server/server/internal/metrics"
	"jt808-server/pkg/protocol"
	"jt808-server/server/internal/session"
	"jt808-server/server/internal/store"
	"time"

	"go.uber.org/zap"
)

// Handler dispatches incoming frames to message-specific logic.
type Handler struct {
	cfg     *config.Config
	reg     *session.Registry
	fwd     *forwarder.Stream
	devices *store.DeviceStore // nil when DB is disabled
	metrics *metrics.Metrics
	log     *zap.Logger
}

func New(cfg *config.Config, reg *session.Registry, fwd *forwarder.Stream, devices *store.DeviceStore, m *metrics.Metrics, log *zap.Logger) *Handler {
	return &Handler{cfg: cfg, reg: reg, fwd: fwd, devices: devices, metrics: m, log: log}
}

func (h *Handler) Dispatch(ctx context.Context, s *session.Session, f *protocol.Frame) {
	h.metrics.FramesReceived.WithLabelValues(msgName(f.MsgID)).Inc()

	if s.State() < session.StateAuthenticated {
		switch f.MsgID {
		case protocol.MsgRegistration, protocol.MsgAuthentication, protocol.MsgTerminalResp,
			protocol.MsgLocationReport, protocol.MsgBatchLocation:
			// allowed before auth
		default:
			h.log.Debug("unauthenticated message dropped",
				zap.String("addr", s.RemoteAddr()),
				zap.String("msg_id_hex", fmt.Sprintf("0x%04X", f.MsgID)),
			)
			s.ACK(f, protocol.ResultOK, h.cfg.WriteTimeout) //nolint:errcheck
			return
		}
	}

	switch f.MsgID {
	case protocol.MsgHeartbeat:
		h.handleHeartbeat(ctx, s, f)
	case protocol.MsgRegistration:
		h.handleRegistration(ctx, s, f)
	case protocol.MsgAuthentication:
		h.handleAuthentication(ctx, s, f)
	case protocol.MsgLocationReport:
		h.handleLocation(ctx, s, f)
	case protocol.MsgBatchLocation:
		h.handleBatchLocation(ctx, s, f)
	case protocol.MsgTerminalResp:
		h.log.Debug("terminal response received", zap.String("phone", s.Phone), zap.Uint16("seq", f.SeqNum))
	case protocol.MsgTerminalProps:
		s.ACK(f, protocol.ResultOK, h.cfg.WriteTimeout) //nolint:errcheck
		h.log.Info("terminal properties",
			zap.String("phone", s.Phone),
			zap.String("body_hex", fmt.Sprintf("%X", f.Body)),
		)
	case protocol.MsgUpgradeResult:
		// Device sends this at startup to confirm no pending firmware upgrade.
		// Empty body is normal; non-empty body encodes upgrade type + result code.
		s.ACK(f, protocol.ResultOK, h.cfg.WriteTimeout) //nolint:errcheck
		h.log.Debug("upgrade result notification",
			zap.String("phone", s.Phone),
			zap.String("body_hex", fmt.Sprintf("%X", f.Body)),
		)
	case protocol.MsgACCStatusReport:
		// Concox GXX dialect: device reports ACC/ignition state at startup.
		// Body byte 0: 0x00 = ACC off, 0x01 = ACC on.
		s.ACK(f, protocol.ResultOK, h.cfg.WriteTimeout) //nolint:errcheck
		accOn := len(f.Body) > 0 && f.Body[0] == 0x01
		h.log.Debug("ACC status report",
			zap.String("phone", s.Phone),
			zap.Bool("acc_on", accOn),
		)
	default:
		h.log.Info("unhandled message",
			zap.String("phone", s.Phone),
			zap.String("msg_id_hex", fmt.Sprintf("0x%04X", f.MsgID)),
			zap.String("body_hex", fmt.Sprintf("%X", f.Body)),
		)
		h.metrics.UnknownMessages.Inc()
		s.ACK(f, protocol.ResultOK, h.cfg.WriteTimeout) //nolint:errcheck
	}
}

func (h *Handler) handleHeartbeat(ctx context.Context, s *session.Session, f *protocol.Frame) {
	h.reg.Heartbeat(ctx, s)
	if err := s.ACK(f, protocol.ResultOK, h.cfg.WriteTimeout); err != nil {
		h.log.Debug("heartbeat ACK write error", zap.String("phone", s.Phone), zap.Error(err))
	}
	h.metrics.Heartbeats.Inc()
	h.log.Debug("heartbeat", zap.String("phone", s.Phone), zap.String("addr", s.RemoteAddr()))
}

func (h *Handler) handleRegistration(ctx context.Context, s *session.Session, f *protocol.Frame) {
	info, err := protocol.DecodeRegistration(f.Body)
	if err != nil {
		h.log.Warn("registration decode error", zap.String("addr", s.RemoteAddr()), zap.Error(err))
		s.ACK(f, protocol.ResultMsgError, h.cfg.WriteTimeout) //nolint:errcheck
		return
	}

	h.log.Info("device registration",
		zap.String("phone", f.Phone),
		zap.String("device_id", info.DeviceID),
		zap.String("model", info.DeviceModel),
	)

	// h.devices is nil when DB is disabled — CheckOrCreate returns CheckApproved.
	result, dbErr := h.devices.CheckOrCreate(ctx, f.Phone, info.DeviceModel, info.DeviceID)
	if dbErr != nil {
		h.log.Error("device approval check failed — rejecting", zap.String("phone", f.Phone), zap.Error(dbErr))
		body := protocol.BuildRegistrationACK(f.SeqNum, protocol.RegTerminalNotInDB, "")
		s.Send(protocol.MsgRegistrationResp, body, h.cfg.WriteTimeout) //nolint:errcheck
		time.AfterFunc(200*time.Millisecond, s.Close)
		return
	}

	switch result {
	case store.CheckAutoCreated:
		h.log.Warn("unknown device auto-created — pending admin approval, rejecting",
			zap.String("imei", f.Phone))
		body := protocol.BuildRegistrationACK(f.SeqNum, protocol.RegTerminalNotInDB, "")
		s.Send(protocol.MsgRegistrationResp, body, h.cfg.WriteTimeout) //nolint:errcheck
		time.AfterFunc(200*time.Millisecond, s.Close)
		return

	case store.CheckNotApproved:
		h.log.Warn("device not approved — rejecting", zap.String("imei", f.Phone))
		body := protocol.BuildRegistrationACK(f.SeqNum, protocol.RegTerminalNotInDB, "")
		s.Send(protocol.MsgRegistrationResp, body, h.cfg.WriteTimeout) //nolint:errcheck
		time.AfterFunc(200*time.Millisecond, s.Close)
		return
	}

	s.Phone = f.Phone
	s.IMEI = info.DeviceID

	token := protocol.GenerateAuthToken(f.Phone)
	if err := h.reg.StoreAuthToken(ctx, f.Phone, token); err != nil {
		h.log.Error("failed to store auth token", zap.String("phone", f.Phone), zap.Error(err))
	}
	s.AuthToken = token
	s.SetState(session.StateRegistered)

	body := protocol.BuildRegistrationACK(f.SeqNum, protocol.RegSuccess, token)
	if err := s.Send(protocol.MsgRegistrationResp, body, h.cfg.WriteTimeout); err != nil {
		h.log.Warn("registration response write error", zap.String("phone", f.Phone), zap.Error(err))
	}

	go h.fwd.PublishEvent(ctx, "device.registered", f.Phone, map[string]any{
		"phone":        f.Phone,
		"device_id":    info.DeviceID,
		"device_model": info.DeviceModel,
		"province_id":  info.ProvinceID,
		"city_id":      info.CityID,
	})
}

func (h *Handler) handleAuthentication(ctx context.Context, s *session.Session, f *protocol.Frame) {
	phone := f.Phone
	token := string(f.Body)

	if s.Phone == "" {
		s.Phone = phone
	}

	valid, err := h.reg.ValidateAuthToken(ctx, phone, token)
	if err != nil {
		h.log.Error("auth token lookup failed", zap.String("phone", phone), zap.Error(err))
		valid = true // fail open on Redis error
	}

	if !valid {
		h.log.Warn("authentication failed — bad token", zap.String("phone", phone), zap.String("addr", s.RemoteAddr()))
		s.ACK(f, protocol.ResultFail, h.cfg.WriteTimeout) //nolint:errcheck
		h.metrics.AuthViolations.Inc()
		time.AfterFunc(500*time.Millisecond, s.Close)
		return
	}

	s.SetState(session.StateAuthenticated)
	h.reg.Register(ctx, s)

	if err := s.ACK(f, protocol.ResultOK, h.cfg.WriteTimeout); err != nil {
		h.log.Warn("auth ACK write error", zap.String("phone", phone), zap.Error(err))
	}

	h.metrics.AuthSuccesses.Inc()
	h.log.Info("device authenticated", zap.String("phone", phone), zap.String("addr", s.RemoteAddr()))

	go h.fwd.PublishEvent(ctx, "device.authenticated", phone, map[string]any{
		"phone":    phone,
		"imei":     s.IMEI,
		"addr":     s.RemoteAddr(),
		"login_at": time.Now().UTC().Format(time.RFC3339),
	})
}

func (h *Handler) handleLocation(ctx context.Context, s *session.Session, f *protocol.Frame) {
	if err := s.ACK(f, protocol.ResultOK, h.cfg.WriteTimeout); err != nil {
		h.log.Debug("location ACK write error", zap.String("phone", s.Phone), zap.Error(err))
		return
	}

	phone := s.Phone
	if phone == "" {
		phone = f.Phone
	}
	if phone == "" {
		return
	}

	loc, err := protocol.DecodeLocation(f.Body)
	if err != nil {
		h.log.Warn("location decode error", zap.String("phone", phone), zap.Error(err))
		h.metrics.DecodeErrors.Inc()
		return
	}

	s.TouchLocation()
	h.metrics.LocationReports.Inc()

	if loc.HasAlarm(protocol.AlarmSOS) {
		h.log.Warn("SOS ALARM",
			zap.String("phone", s.Phone),
			zap.Float64("lat", loc.Latitude),
			zap.Float64("lon", loc.Longitude),
		)
		h.metrics.SOSAlarms.Inc()
	}

	go h.fwd.PublishLocation(ctx, phone, loc)
}

func (h *Handler) handleBatchLocation(ctx context.Context, s *session.Session, f *protocol.Frame) {
	if err := s.ACK(f, protocol.ResultOK, h.cfg.WriteTimeout); err != nil {
		return
	}
	if len(f.Body) < 3 {
		return
	}

	count := (int(f.Body[0]) << 8) | int(f.Body[1])
	offset := 3
	for i := 0; i < count && offset+2 <= len(f.Body); i++ {
		itemLen := (int(f.Body[offset]) << 8) | int(f.Body[offset+1])
		offset += 2
		if offset+itemLen > len(f.Body) {
			break
		}
		body := f.Body[offset : offset+itemLen]
		offset += itemLen

		loc, err := protocol.DecodeLocation(body)
		if err != nil {
			h.metrics.DecodeErrors.Inc()
			continue
		}
		h.metrics.LocationReports.Inc()
		go h.fwd.PublishLocation(ctx, s.Phone, loc)
	}

	h.log.Debug("batch location", zap.String("phone", s.Phone), zap.Int("count", count))
}

func msgName(id uint16) string {
	switch id {
	case protocol.MsgHeartbeat:
		return "heartbeat"
	case protocol.MsgRegistration:
		return "registration"
	case protocol.MsgAuthentication:
		return "authentication"
	case protocol.MsgLocationReport:
		return "location"
	case protocol.MsgBatchLocation:
		return "batch_location"
	case protocol.MsgTerminalResp:
		return "terminal_resp"
	case protocol.MsgTerminalProps:
		return "terminal_props"
	case protocol.MsgUpgradeResult:
		return "upgrade_result"
	case protocol.MsgACCStatusReport:
		return "acc_status"
	default:
		return "unknown"
	}
}
