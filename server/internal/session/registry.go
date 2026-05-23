package session

import (
	"context"
	"fmt"
	"jt808-server/server/internal/config"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Registry manages the local connection map and synchronises state to Redis.
type Registry struct {
	mu              sync.RWMutex
	local           map[string]*Session
	rdb             *redis.Client
	log             *zap.Logger
	prefix          string
	onlineZ         string
	authTokenPrefix string
}

func NewRegistry(cfg *config.Config, rdb *redis.Client, log *zap.Logger) *Registry {
	return &Registry{
		local:           make(map[string]*Session),
		rdb:             rdb,
		log:             log,
		prefix:          cfg.SessionPrefix,
		onlineZ:         cfg.OnlineZKey,
		authTokenPrefix: cfg.AuthTokenPrefix,
	}
}

func (r *Registry) Register(ctx context.Context, s *Session) {
	r.mu.Lock()
	if old, ok := r.local[s.Phone]; ok && old != s {
		old.Close()
		delete(r.local, s.Phone)
	}
	r.local[s.Phone] = s
	r.mu.Unlock()

	go func() {
		key := r.prefix + s.Phone
		now := time.Now()
		pipe := r.rdb.Pipeline()
		pipe.HSet(ctx, key,
			"phone", s.Phone,
			"imei", s.IMEI,
			"connected_at", now.Unix(),
			"last_heartbeat", now.Unix(),
		)
		pipe.Expire(ctx, key, 24*time.Hour)
		pipe.ZAdd(ctx, r.onlineZ, redis.Z{Score: float64(now.UnixNano()), Member: s.Phone})
		if _, err := pipe.Exec(ctx); err != nil {
			r.log.Warn("registry: redis register failed", zap.String("phone", s.Phone), zap.Error(err))
		}
	}()
}

func (r *Registry) Unregister(ctx context.Context, s *Session) {
	if s.Phone == "" {
		return
	}
	r.mu.Lock()
	if r.local[s.Phone] == s {
		delete(r.local, s.Phone)
	}
	r.mu.Unlock()

	go func() {
		pipe := r.rdb.Pipeline()
		pipe.Del(ctx, r.prefix+s.Phone)
		pipe.ZRem(ctx, r.onlineZ, s.Phone)
		if _, err := pipe.Exec(ctx); err != nil {
			r.log.Warn("registry: redis unregister failed", zap.String("phone", s.Phone), zap.Error(err))
		}
	}()
}

func (r *Registry) Heartbeat(ctx context.Context, s *Session) {
	s.Touch()
	go func() {
		now := time.Now()
		pipe := r.rdb.Pipeline()
		pipe.HSet(ctx, r.prefix+s.Phone, "last_heartbeat", now.Unix())
		pipe.Expire(ctx, r.prefix+s.Phone, 24*time.Hour)
		pipe.ZAdd(ctx, r.onlineZ, redis.Z{Score: float64(now.UnixNano()), Member: s.Phone})
		if _, err := pipe.Exec(ctx); err != nil {
			r.log.Warn("registry: redis heartbeat failed", zap.String("phone", s.Phone), zap.Error(err))
		}
	}()
}

func (r *Registry) StoreAuthToken(ctx context.Context, phone, token string) error {
	return r.rdb.Set(ctx, fmt.Sprintf("%s%s", r.authTokenPrefix, phone), token, 24*time.Hour).Err()
}

func (r *Registry) ValidateAuthToken(ctx context.Context, phone, token string) (bool, error) {
	stored, err := r.rdb.Get(ctx, fmt.Sprintf("%s%s", r.authTokenPrefix, phone)).Result()
	if err == redis.Nil {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return stored == token, nil
}

func (r *Registry) Get(phone string) *Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.local[phone]
}

func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.local)
}

func (r *Registry) OnlineCount(ctx context.Context) (int64, error) {
	return r.rdb.ZCard(ctx, r.onlineZ).Result()
}

func (r *Registry) PruneStale(ctx context.Context, ttl time.Duration) error {
	cutoff := float64(time.Now().Add(-ttl).UnixNano())
	return r.rdb.ZRemRangeByScore(ctx, r.onlineZ, "-inf", fmt.Sprintf("%f", cutoff)).Err()
}

func (r *Registry) ForEachLocal(fn func(*Session)) {
	r.mu.RLock()
	sessions := make([]*Session, 0, len(r.local))
	for _, s := range r.local {
		sessions = append(sessions, s)
	}
	r.mu.RUnlock()
	for _, s := range sessions {
		fn(s)
	}
}
