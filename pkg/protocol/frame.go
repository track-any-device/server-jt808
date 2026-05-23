// Package protocol implements the JT/T 808-2019 binary framing protocol.
//
// Frame wire format (after unescape):
//
//	0x7e | MsgID(2) | Attrs(2) | Phone-BCD(6) | SeqNum(2) [| TotalPkg(2) | PkgSeq(2)] | Body(N) | Checksum(1) | 0x7e
//
// Escape rules (applied to everything between the two 0x7e flags):
//
//	0x7e → 0x7d 0x02
//	0x7d → 0x7d 0x01
//
// Checksum: XOR of all bytes from MsgID through Body (inclusive), before escaping.
package protocol

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	flagByte   byte = 0x7e
	escapeByte byte = 0x7d
)

// Frame is a fully decoded JT808 frame — unescaped and checksum-validated.
type Frame struct {
	MsgID  uint16
	Phone  string // 12-digit BCD-decoded, e.g. "008613987654321"
	SeqNum uint16
	Body   []byte

	// Fragmentation (Attrs bit 13 set)
	IsSubpackage bool
	TotalPkg     uint16
	PkgSeq       uint16
}

// ReadFrame reads one complete JT808 frame from r.
//
// Design: reads byte-by-byte to avoid over-reading from a buffered stream shared
// with multiple goroutines. Each connection has exactly one goroutine calling
// ReadFrame, so this is safe and correct.
//
// Returns io.EOF if the peer closed cleanly before sending any byte of a frame.
// Returns a wrapped error for mid-frame disconnects or protocol violations.
func ReadFrame(r io.Reader) (*Frame, error) {
	one := make([]byte, 1)

	// Skip until we see an opening flag byte.
	// Devices can send garbage bytes on reconnect; this is resilient to that.
	for {
		if _, err := io.ReadFull(r, one); err != nil {
			return nil, err // io.EOF propagates naturally here
		}
		if one[0] == flagByte {
			break
		}
	}

	// Collect the frame body until the closing 0x7e, undoing escape sequences.
	var raw []byte
	for {
		if _, err := io.ReadFull(r, one); err != nil {
			return nil, fmt.Errorf("mid-frame disconnect: %w", err)
		}
		b := one[0]
		if b == flagByte {
			break // closing flag — frame is complete
		}
		if b == escapeByte {
			if _, err := io.ReadFull(r, one); err != nil {
				return nil, fmt.Errorf("escape byte read: %w", err)
			}
			switch one[0] {
			case 0x01:
				raw = append(raw, 0x7d)
			case 0x02:
				raw = append(raw, 0x7e)
			default:
				return nil, fmt.Errorf("invalid escape sequence 0x7d 0x%02x", one[0])
			}
			continue
		}
		raw = append(raw, b)
	}

	// Minimum: 12 header bytes + 1 checksum byte
	if len(raw) < 13 {
		return nil, fmt.Errorf("frame too short (%d bytes)", len(raw))
	}

	// Validate checksum: XOR of all bytes except the trailing checksum byte.
	payload := raw[:len(raw)-1]
	want := raw[len(raw)-1]
	if got := xorChecksum(payload); got != want {
		return nil, fmt.Errorf("checksum mismatch: computed 0x%02x, frame 0x%02x", got, want)
	}

	// Parse header fields.
	rd := bytes.NewReader(payload)
	var msgID, attrs, seqNum uint16
	binary.Read(rd, binary.BigEndian, &msgID)  //nolint:errcheck
	binary.Read(rd, binary.BigEndian, &attrs)  //nolint:errcheck
	phoneBCD := make([]byte, 6)
	rd.Read(phoneBCD) //nolint:errcheck
	binary.Read(rd, binary.BigEndian, &seqNum) //nolint:errcheck

	f := &Frame{
		MsgID:  msgID,
		Phone:  decodeBCD(phoneBCD),
		SeqNum: seqNum,
	}

	// Fragmented message: Attrs bit 13 set → four extra header bytes.
	if attrs&0x2000 != 0 {
		f.IsSubpackage = true
		binary.Read(rd, binary.BigEndian, &f.TotalPkg) //nolint:errcheck
		binary.Read(rd, binary.BigEndian, &f.PkgSeq)   //nolint:errcheck
	}

	f.Body = make([]byte, rd.Len())
	rd.Read(f.Body) //nolint:errcheck

	return f, nil
}

// WriteFrame encodes a JT808 frame and writes it to w in one call.
// Thread-safe: callers must hold the connection write lock.
func WriteFrame(w io.Writer, msgID uint16, phone string, seqNum uint16, body []byte) error {
	// Build raw payload (header + body).
	hdr := make([]byte, 12)
	binary.BigEndian.PutUint16(hdr[0:2], msgID)
	binary.BigEndian.PutUint16(hdr[2:4], uint16(len(body))&0x03FF) // attrs: length in bits 0-9
	copy(hdr[4:10], encodeBCD(phone))
	binary.BigEndian.PutUint16(hdr[10:12], seqNum)

	raw := make([]byte, 0, len(hdr)+len(body)+1)
	raw = append(raw, hdr...)
	raw = append(raw, body...)
	raw = append(raw, xorChecksum(raw))

	// Escape and wrap in delimiters.
	var out bytes.Buffer
	out.Grow(len(raw) + 8)
	out.WriteByte(flagByte)
	for _, b := range raw {
		switch b {
		case 0x7e:
			out.WriteByte(escapeByte)
			out.WriteByte(0x02)
		case 0x7d:
			out.WriteByte(escapeByte)
			out.WriteByte(0x01)
		default:
			out.WriteByte(b)
		}
	}
	out.WriteByte(flagByte)

	_, err := w.Write(out.Bytes())
	return err
}

// BuildPlatformACK returns the body for a 0x8001 platform general response.
// replySeq and replyMsgID identify the message being acknowledged.
// result: 0=success, 1=fail, 2=message error, 3=unsupported, 4=alarm ACK.
func BuildPlatformACK(replySeq, replyMsgID uint16, result byte) []byte {
	b := make([]byte, 5)
	binary.BigEndian.PutUint16(b[0:2], replySeq)
	binary.BigEndian.PutUint16(b[2:4], replyMsgID)
	b[4] = result
	return b
}

// BuildRegistrationACK returns the body for a 0x8100 registration response.
// token is the auth string the device must present in its 0x0102 message.
func BuildRegistrationACK(replySeq uint16, result byte, token string) []byte {
	b := make([]byte, 3+len(token))
	binary.BigEndian.PutUint16(b[0:2], replySeq)
	b[2] = result
	copy(b[3:], token)
	return b
}

func xorChecksum(data []byte) byte {
	var cs byte
	for _, b := range data {
		cs ^= b
	}
	return cs
}

// decodeBCD converts 6 BCD-encoded bytes into a phone number string.
// Trailing 0xf nibbles (padding) are stripped.
func decodeBCD(b []byte) string {
	digits := make([]byte, 0, 12)
	for _, v := range b {
		hi := (v >> 4) & 0x0f
		lo := v & 0x0f
		if hi != 0x0f {
			digits = append(digits, '0'+hi)
		}
		if lo != 0x0f {
			digits = append(digits, '0'+lo)
		}
	}
	return string(digits)
}

// encodeBCD encodes a phone string (up to 12 digits) into 6 BCD bytes.
// Unused nibbles are padded with 0xf.
func encodeBCD(phone string) []byte {
	b := [6]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	for i, ch := range phone {
		if i >= 12 {
			break
		}
		nibble := byte(ch - '0')
		if i%2 == 0 {
			b[i/2] = (nibble << 4) | 0x0f
		} else {
			b[i/2] = (b[i/2] & 0xf0) | nibble
		}
	}
	return b[:]
}
