package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration loaded from environment variables.
// Every field has a safe default so the server works out of the box in dev.
type Config struct {
	// TCP listener — JT808 devices connect here
	TCPAddr string

	// Redis connection
	RedisAddr     string
	RedisPassword string
	RedisDB       int
	RedisPoolSize int

	// Redis key layout — all controllable via env so multiple environments
	// (dev / staging / prod) can share one Redis instance without key collisions.
	StreamKey       string
	StreamMaxLen    int64
	SessionPrefix   string
	AuthTokenPrefix string
	OnlineZKey      string
	CmdChannel      string

	// Protocol timing
	AuthTimeout      time.Duration
	HeartbeatTimeout time.Duration
	WriteTimeout     time.Duration

	// MySQL — optional; disabled when DB_HOST is not set.
	// Set DB_ENABLED=false to explicitly disable regardless of other DB vars.
	DBEnabled      bool
	DBHost         string
	DBPort         string
	DBUser         string
	DBPassword     string
	DBName         string
	DBDeviceTypeID int64

	// Configurable table / column names so this server can attach to any
	// existing Laravel (or other) schema without migration.
	DBDevicesTable    string // DB_DEVICES_TABLE      default "devices"
	DBIMEIColumn      string // DB_IMEI_COLUMN         default "imei"
	DBBroadcastColumn string // DB_BROADCAST_COLUMN    default "broadcast_id"
	DBStatusColumn    string // DB_STATUS_COLUMN       default "status"
	DBTypeIDColumn    string // DB_TYPE_ID_COLUMN      default "device_type_id"
	DBNameColumn      string // DB_NAME_COLUMN         default "name"
	DBNotesColumn     string // DB_NOTES_COLUMN        default "notes"
	DBCreatedAtColumn string // DB_CREATED_AT_COLUMN   default "created_at"
	DBUpdatedAtColumn string // DB_UPDATED_AT_COLUMN   default "updated_at"

	// Observability HTTP (health + Prometheus /metrics)
	HTTPAddr string

	// Runtime identity
	Debug    bool
	ServerID string
}

// MySQLDSN builds the go-sql-driver/mysql DSN from the DB_* fields.
func (c *Config) MySQLDSN() string {
	return fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&timeout=5s&readTimeout=5s&writeTimeout=5s",
		c.DBUser, c.DBPassword, c.DBHost, c.DBPort, c.DBName,
	)
}

func Load() *Config {
	// DB is enabled when DB_HOST is explicitly set, OR DB_ENABLED=true is set.
	// DB_ENABLED=false always overrides.
	dbHostSet := os.Getenv("DB_HOST") != ""
	dbEnabled := envBool("DB_ENABLED", dbHostSet)

	return &Config{
		TCPAddr:          envStr("JT808_TCP_ADDR", ":7018"),
		RedisAddr:        envStr("REDIS_HOST", "redis") + ":" + envStr("REDIS_PORT", "6379"),
		RedisPassword:    envStr("REDIS_PASSWORD", ""),
		RedisDB:          envInt("REDIS_JT808_DB", 0),
		RedisPoolSize:    envInt("REDIS_POOL_SIZE", 100),
		StreamKey:        envStr("STREAM_KEY", "jt808:telemetry"),
		StreamMaxLen:     envInt64("STREAM_MAX_LEN", 100_000),
		SessionPrefix:    envStr("SESSION_PREFIX", "jt808:session:"),
		AuthTokenPrefix:  envStr("AUTH_TOKEN_PREFIX", "jt808:authtoken:"),
		OnlineZKey:       envStr("ONLINE_Z_KEY", "jt808:online"),
		CmdChannel:       envStr("CMD_CHANNEL", "jt808:cmd:"),
		AuthTimeout:      envDuration("AUTH_TIMEOUT", 30*time.Second),
		HeartbeatTimeout: envDuration("HEARTBEAT_TIMEOUT", 3*time.Minute),
		WriteTimeout:     envDuration("WRITE_TIMEOUT", 10*time.Second),
		HTTPAddr:         envStr("JT808_HTTP_ADDR", ":9090"),
		Debug:            envStr("APP_DEBUG", "false") == "true",
		ServerID:         envStr("SERVER_ID", mustHostname()),

		DBEnabled:      dbEnabled,
		DBHost:         envStr("DB_HOST", "mysql"),
		DBPort:         envStr("DB_PORT", "3306"),
		DBUser:         envStr("DB_USERNAME", "laravel"),
		DBPassword:     envStr("DB_PASSWORD", ""),
		DBName:         envStr("DB_DATABASE", "laravel"),
		DBDeviceTypeID: envInt64("DB_DEVICE_TYPE_ID", 1),

		DBDevicesTable:    envStr("DB_DEVICES_TABLE", "devices"),
		DBIMEIColumn:      envStr("DB_IMEI_COLUMN", "imei"),
		DBBroadcastColumn: envStr("DB_BROADCAST_COLUMN", "broadcast_id"),
		DBStatusColumn:    envStr("DB_STATUS_COLUMN", "status"),
		DBTypeIDColumn:    envStr("DB_TYPE_ID_COLUMN", "device_type_id"),
		DBNameColumn:      envStr("DB_NAME_COLUMN", "name"),
		DBNotesColumn:     envStr("DB_NOTES_COLUMN", "notes"),
		DBCreatedAtColumn: envStr("DB_CREATED_AT_COLUMN", "created_at"),
		DBUpdatedAtColumn: envStr("DB_UPDATED_AT_COLUMN", "updated_at"),
	}
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
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

func mustHostname() string {
	h, _ := os.Hostname()
	return h
}
