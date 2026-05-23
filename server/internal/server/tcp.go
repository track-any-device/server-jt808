// Package server runs the JT808 TCP listener and the HTTP observability server.
package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"jt808-server/server/internal/config"
	"jt808-server/server/internal/forwarder"
	"jt808-server/server/internal/handler"
	"jt808-server/server/internal/metrics"
	"jt808-server/pkg/protocol"
	"jt808-server/server/internal/session"
	"jt808-server/server/internal/store"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// Server wraps the TCP listener and the HTTP health/metrics server.
type Server struct {
	cfg     *config.Config
	reg     *session.Registry
	handler *handler.Handler
	metrics *metrics.Metrics
	log     *zap.Logger
}

func New(cfg *config.Config, reg *session.Registry, fwd *forwarder.Stream, devices *store.DeviceStore, m *metrics.Metrics, log *zap.Logger) *Server {
	h := handler.New(cfg, reg, fwd, devices, m, log)
	return &Server{cfg: cfg, reg: reg, handler: h, metrics: m, log: log}
}

// Run starts the TCP listener and HTTP server, blocking until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	lc := net.ListenConfig{KeepAlive: 60 * time.Second}

	ln, err := lc.Listen(ctx, "tcp", s.cfg.TCPAddr)
	if err != nil {
		return err
	}
	tcpLn := ln.(*net.TCPListener)
	s.log.Info("JT808 TCP listening", zap.String("addr", s.cfg.TCPAddr))

	httpSrv := s.startHTTP(ctx)
	go s.pruneLoop(ctx)

	go func() {
		<-ctx.Done()
		tcpLn.Close()
	}()

	for {
		conn, err := tcpLn.AcceptTCP()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			s.log.Error("accept error", zap.Error(err))
			time.Sleep(50 * time.Millisecond)
			continue
		}

		conn.SetKeepAlive(true)
		conn.SetKeepAlivePeriod(60 * time.Second)
		s.metrics.ConnectionsTotal.Inc()
		s.metrics.ConnectionsActive.Inc()
		go s.handleConn(ctx, conn)
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutCtx) //nolint:errcheck

	s.log.Info("all connections drained; server stopped")
	return nil
}

func (s *Server) handleConn(ctx context.Context, conn *net.TCPConn) {
	defer func() {
		conn.Close()
		s.metrics.ConnectionsActive.Dec()
	}()

	sess := session.NewSession(conn)
	defer s.reg.Unregister(ctx, sess)

	s.log.Info("device connected", zap.String("addr", sess.RemoteAddr()))

	authTimer := time.AfterFunc(s.cfg.AuthTimeout, func() {
		if sess.State() < session.StateAuthenticated {
			s.log.Warn("auth timeout — closing unauthenticated connection",
				zap.String("addr", sess.RemoteAddr()))
			sess.Close()
		}
	})
	defer authTimer.Stop()

	r := bufio.NewReaderSize(conn, 4096)
	for {
		conn.SetReadDeadline(time.Now().Add(s.cfg.HeartbeatTimeout))

		frame, err := protocol.ReadFrame(r)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if isTimeout(err) {
				s.log.Info("device heartbeat timeout",
					zap.String("phone", sess.Phone),
					zap.String("addr", sess.RemoteAddr()))
			} else {
				s.log.Debug("connection closed",
					zap.String("phone", sess.Phone),
					zap.String("addr", sess.RemoteAddr()),
					zap.Error(err))
			}
			return
		}
		s.handler.Dispatch(ctx, sess, frame)
	}
}

func (s *Server) startHTTP(ctx context.Context) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok")) //nolint:errcheck
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ready")) //nolint:errcheck
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"service":"jt808-server","tcp_port":7018,"status":"ok","endpoints":["/healthz","/readyz","/metrics"]}`) //nolint:errcheck
	})

	srv := &http.Server{Addr: s.cfg.HTTPAddr, Handler: mux}
	go func() {
		s.log.Info("HTTP observability listening", zap.String("addr", s.cfg.HTTPAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("HTTP server error", zap.Error(err))
		}
	}()
	return srv
}

func (s *Server) pruneLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.HeartbeatTimeout)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.reg.PruneStale(ctx, s.cfg.HeartbeatTimeout*2); err != nil {
				s.log.Warn("stale session prune error", zap.Error(err))
			}
		}
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
