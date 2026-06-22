// Package store provides access to the MySQL database for device lookup and
// auto-registration.  The store is optional — when DB is disabled, all
// connecting devices are automatically approved.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"jt808-server/server/internal/config"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"go.uber.org/zap"
)

// DeviceStore looks up and auto-creates devices in the MySQL database.
// A nil *DeviceStore is valid and approves every device (DB-less mode).
type DeviceStore struct {
	db  *sql.DB
	cfg *config.Config
	log *zap.Logger
}

// New opens a MySQL connection pool.
func New(cfg *config.Config, log *zap.Logger) (*DeviceStore, error) {
	db, err := sql.Open("mysql", cfg.MySQLDSN())
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	return &DeviceStore{db: db, cfg: cfg, log: log}, nil
}

// Ping verifies the database is reachable.
func (s *DeviceStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// CheckResult is returned by CheckOrCreate.
type CheckResult int

const (
	CheckApproved    CheckResult = iota // device exists and is not blocked → allow
	CheckBlocked                        // device exists but status = blocked → reject
	CheckAutoCreated                    // device was just inserted as pending → allow (approved=false)
)

// CheckOrCreate looks up the device by its broadcast id (the id the device
// emits on connect). A new device is auto-created as `pending` (only the
// broadcast id is known at this point) and allowed to connect; a device whose
// status is `blocked` is rejected. Everything else (pending/active) is allowed.
//
// If s is nil (DB disabled) every device is treated as approved.
func (s *DeviceStore) CheckOrCreate(ctx context.Context, broadcastID, model, deviceID string) (CheckResult, error) {
	if s == nil {
		return CheckApproved, nil
	}

	var status string
	err := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s FROM %s WHERE %s = ? LIMIT 1`,
			s.cfg.DBStatusColumn,
			s.cfg.DBDevicesTable,
			s.cfg.DBBroadcastColumn,
		), broadcastID,
	).Scan(&status)

	if err == nil {
		if status == "blocked" {
			return CheckBlocked, nil
		}
		return CheckApproved, nil
	}

	if err != sql.ErrNoRows {
		return CheckBlocked, fmt.Errorf("store: lookup broadcast_id %s: %w", broadcastID, err)
	}

	// Device not found — auto-create as pending (only the broadcast id is known).
	name := deviceID
	if name == "" {
		name = broadcastID
	}
	notes := fmt.Sprintf("Auto-registered via JT808. Model: %s  DeviceID: %s", model, deviceID)
	now := time.Now().UTC().Format("2006-01-02 15:04:05")

	_, err = s.db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s
			(%s, %s, %s, %s, %s, %s, %s)
		VALUES
			(?, ?, ?, 'pending', ?, ?, ?)
	`,
		s.cfg.DBDevicesTable,
		s.cfg.DBTypeIDColumn,
		s.cfg.DBBroadcastColumn,
		s.cfg.DBNameColumn,
		s.cfg.DBStatusColumn,
		s.cfg.DBNotesColumn,
		s.cfg.DBCreatedAtColumn,
		s.cfg.DBUpdatedAtColumn,
	), s.cfg.DBDeviceTypeID, broadcastID, name, notes, now, now)
	if err != nil {
		return CheckBlocked, fmt.Errorf("store: auto-create broadcast_id %s: %w", broadcastID, err)
	}

	s.log.Info("device auto-created — pending",
		zap.String("broadcast_id", broadcastID),
		zap.String("model", model),
	)
	return CheckAutoCreated, nil
}
