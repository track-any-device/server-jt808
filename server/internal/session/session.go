package session

import (
	"jt808-server/pkg/protocol"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type State int32

const (
	StateConnected     State = iota
	StateRegistered
	StateAuthenticated
	StateClosing
)

// Session represents one active TCP connection.
type Session struct {
	conn    net.Conn
	writeMu sync.Mutex

	Phone string
	IMEI  string

	AuthToken string

	state  atomic.Int32
	outSeq atomic.Uint32

	ConnectedAt   time.Time
	LastHeartbeat atomic.Int64
	LastLocation  atomic.Int64
}

func NewSession(conn net.Conn) *Session {
	s := &Session{
		conn:        conn,
		ConnectedAt: time.Now(),
	}
	s.state.Store(int32(StateConnected))
	s.LastHeartbeat.Store(time.Now().UnixNano())
	return s
}

func (s *Session) State() State {
	return State(s.state.Load())
}

func (s *Session) SetState(st State) {
	s.state.Store(int32(st))
}

func (s *Session) Send(msgID uint16, body []byte, deadline time.Duration) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	seq := uint16(s.outSeq.Add(1) & 0xffff)
	s.conn.SetWriteDeadline(time.Now().Add(deadline))
	return protocol.WriteFrame(s.conn, msgID, s.Phone, seq, body)
}

func (s *Session) ACK(f *protocol.Frame, result byte, writeDeadline time.Duration) error {
	body := protocol.BuildPlatformACK(f.SeqNum, f.MsgID, result)
	return s.Send(protocol.MsgPlatformResp, body, writeDeadline)
}

func (s *Session) Touch() {
	s.LastHeartbeat.Store(time.Now().UnixNano())
}

func (s *Session) TouchLocation() {
	s.LastLocation.Store(time.Now().UnixNano())
}

func (s *Session) IsStale(ttl time.Duration) bool {
	last := time.Unix(0, s.LastHeartbeat.Load())
	return time.Since(last) > ttl
}

func (s *Session) RemoteAddr() string {
	return s.conn.RemoteAddr().String()
}

func (s *Session) SetReadDeadline(d time.Time) error {
	return s.conn.SetReadDeadline(d)
}

func (s *Session) Close() {
	s.state.Store(int32(StateClosing))
	s.conn.Close()
}
