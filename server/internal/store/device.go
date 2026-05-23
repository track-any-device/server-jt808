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
	CheckApproved    CheckResult = iota // device exists and approved column = true
	CheckNotApproved                    // device exists but not approved
	CheckAutoCreated                    // device was just inserted (not approved)
)

// CheckOrCreate looks up the device by IMEI.
//
// If s is nil (DB disabled) every device is treated as approved.
func (s *DeviceStore) CheckOrCreate(ctx context.Context, imei, model, deviceID string) (CheckResult, error) {
	if s == nil {
		return CheckApproved, nil
	}

	var isApproved bool
	err := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s FROM %s WHERE %s = ? LIMIT 1`,
			s.cfg.DBApprovedColumn,
			s.cfg.DBDevicesTable,
			s.cfg.DBIMEIColumn,
		), imei,
	).Scan(&isApproved)

	if err == nil {
		if isApproved {
			return CheckApproved, nil
		}
		return CheckNotApproved, nil
	}

	if err != sql.ErrNoRows {
		return CheckNotApproved, fmt.Errorf("store: lookup imei %s: %w", imei, err)
	}

	// Device not found — auto-create in pending-approval state.
	name := fmt.Sprintf("JT808 %s", imei)
	notes := fmt.Sprintf("Auto-registered via JT808. Model: %s  DeviceID: %s", model, deviceID)
	now := time.Now().UTC().Format("2006-01-02 15:04:05")

	_, err = s.db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s
			(%s, %s, %s, %s, %s, %s, %s, %s)
		VALUES
			(?, ?, ?, 'inventory', 0, ?, ?, ?)
	`,
		s.cfg.DBDevicesTable,
		s.cfg.DBTypeIDColumn,
		s.cfg.DBIMEIColumn,
		s.cfg.DBNameColumn,
		s.cfg.DBStatusColumn,
		s.cfg.DBApprovedColumn,
		s.cfg.DBNotesColumn,
		s.cfg.DBCreatedAtColumn,
		s.cfg.DBUpdatedAtColumn,
	), s.cfg.DBDeviceTypeID, imei, name, notes, now, now)
	if err != nil {
		return CheckNotApproved, fmt.Errorf("store: auto-create imei %s: %w", imei, err)
	}

	s.log.Info("device auto-created — pending approval",
		zap.String("imei", imei),
		zap.String("model", model),
	)
	return CheckAutoCreated, nil
}
