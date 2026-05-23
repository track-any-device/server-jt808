package main

import (
	"context"
	"jt808-server/server/internal/config"
	"jt808-server/server/internal/forwarder"
	"jt808-server/server/internal/metrics"
	"jt808-server/server/internal/server"
	"jt808-server/server/internal/session"
	"jt808-server/server/internal/store"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

func main() {
	cfg := config.Load()

	var log *zap.Logger
	if cfg.Debug {
		log, _ = zap.NewDevelopment()
	} else {
		log, _ = zap.NewProduction()
	}
	defer log.Sync() //nolint:errcheck

	if cfg.DBEnabled {
		log.Info("database enabled", zap.String("host", cfg.DBHost), zap.String("db", cfg.DBName))
	} else {
		log.Info("database disabled — all registrations auto-approved")
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.RedisAddr,
		Password:     cfg.RedisPassword,
		DB:           cfg.RedisDB,
		PoolSize:     cfg.RedisPoolSize,
		MinIdleConns: 5,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Wait for Redis.
	for {
		if err := rdb.Ping(ctx).Err(); err != nil {
			log.Warn("Redis not ready, retrying in 2s", zap.String("addr", cfg.RedisAddr), zap.Error(err))
			select {
			case <-ctx.Done():
				log.Fatal("context cancelled waiting for Redis")
			case <-time.After(2 * time.Second):
			}
			continue
		}
		break
	}
	log.Info("Redis connected", zap.String("addr", cfg.RedisAddr))

	// Wait for MySQL only when enabled.
	var devices *store.DeviceStore
	if cfg.DBEnabled {
		for {
			var err error
			devices, err = store.New(cfg, log)
			if err != nil {
				log.Warn("MySQL not ready, retrying in 2s", zap.String("host", cfg.DBHost), zap.Error(err))
			} else if err = devices.Ping(ctx); err != nil {
				log.Warn("MySQL ping failed, retrying in 2s", zap.String("host", cfg.DBHost), zap.Error(err))
			} else {
				break
			}
			select {
			case <-ctx.Done():
				log.Fatal("context cancelled waiting for MySQL")
			case <-time.After(2 * time.Second):
			}
		}
		log.Info("MySQL connected", zap.String("host", cfg.DBHost), zap.String("db", cfg.DBName))
	}

	reg := session.NewRegistry(cfg, rdb, log)
	fwd := forwarder.NewStream(rdb, cfg.StreamKey, cfg.StreamMaxLen, log)
	m := metrics.New()
	srv := server.New(cfg, reg, fwd, devices, m, log)

	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		sig := <-ch
		log.Info("shutdown signal received", zap.String("signal", sig.String()))
		cancel()
	}()

	if err := srv.Run(ctx); err != nil {
		log.Error("server exited with error", zap.Error(err))
		os.Exit(1)
	}
}
