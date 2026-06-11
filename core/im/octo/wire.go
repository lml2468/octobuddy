package octo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"
)

// WuKongIM binary framing (wire-compatible with cc-channel-octo src/octo/socket.ts).
//
// Frame = header byte | remaining-length varint | body.
// Header byte = (packetType << 4) | flags.
// All fixed-width integers are BIG-ENDIAN. Strings are uint16-BE byte-length
// prefixed UTF-8. Remaining length is an MQTT base-128 varint (max 4 bytes).

type packetType byte

const (
	pktConnect    packetType = 1
	pktConnack    packetType = 2
	pktSend       packetType = 3
	pktSendack    packetType = 4
	pktRecv       packetType = 5
	pktRecvack    packetType = 6
	pktPing       packetType = 7
	pktPong       packetType = 8
	pktDisconnect packetType = 9
)

const protoVersion = 4
const maxVarlenBytes = 4

// --- encoder ---

type encoder struct{ buf []byte }

func (e *encoder) writeByte(b byte)    { e.buf = append(e.buf, b) }
func (e *encoder) writeBytes(b []byte) { e.buf = append(e.buf, b...) }

func (e *encoder) writeInt16(v uint16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	e.buf = append(e.buf, b[:]...)
}

func (e *encoder) writeInt32(v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	e.buf = append(e.buf, b[:]...)
}

func (e *encoder) writeInt64(v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	e.buf = append(e.buf, b[:]...)
}

// writeString writes a uint16-BE byte-length prefix followed by UTF-8 bytes.
// The wire format caps a string at 65535 bytes; clamp (on a rune boundary) when
// a field somehow exceeds that, so the length prefix can never desync from the
// payload and corrupt every subsequent field in the frame.
func (e *encoder) writeString(s string) {
	b := []byte(s)
	if len(b) > maxStringBytes {
		cut := maxStringBytes
		for cut > 0 && !utf8.RuneStart(b[cut]) {
			cut--
		}
		b = b[:cut]
	}
	e.writeInt16(uint16(len(b)))
	e.writeBytes(b)
}

const maxStringBytes = 65535 // uint16 max — the wire length-prefix ceiling

// encodeVariableLength encodes an MQTT base-128 varint (socket.ts).
func encodeVariableLength(n int) []byte {
	if n == 0 {
		return []byte{0}
	}
	var out []byte
	for n > 0 {
		digit := byte(n % 0x80)
		n /= 0x80
		if n > 0 {
			digit |= 0x80
		}
		out = append(out, digit)
	}
	return out
}

// frame wraps a body with a packet header + remaining-length varint.
func frame(pt packetType, body []byte) []byte {
	out := []byte{byte(pt)<<4 | 0}
	out = append(out, encodeVariableLength(len(body))...)
	out = append(out, body...)
	return out
}

// --- decoder ---

type decoder struct {
	buf []byte
	pos int
}

var errShort = errors.New("octo: short read")

func (d *decoder) readByte() (byte, error) {
	if d.pos >= len(d.buf) {
		return 0, errShort
	}
	b := d.buf[d.pos]
	d.pos++
	return b, nil
}

func (d *decoder) readInt16() (uint16, error) {
	if d.pos+2 > len(d.buf) {
		return 0, errShort
	}
	v := binary.BigEndian.Uint16(d.buf[d.pos:])
	d.pos += 2
	return v, nil
}

func (d *decoder) readInt32() (uint32, error) {
	if d.pos+4 > len(d.buf) {
		return 0, errShort
	}
	v := binary.BigEndian.Uint32(d.buf[d.pos:])
	d.pos += 4
	return v, nil
}

func (d *decoder) readInt64() (uint64, error) {
	if d.pos+8 > len(d.buf) {
		return 0, errShort
	}
	v := binary.BigEndian.Uint64(d.buf[d.pos:])
	d.pos += 8
	return v, nil
}

// readString reads a uint16-BE length prefix and that many UTF-8 bytes.
func (d *decoder) readString() (string, error) {
	n, err := d.readInt16()
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", nil
	}
	if d.pos+int(n) > len(d.buf) {
		return "", errShort
	}
	s := string(d.buf[d.pos : d.pos+int(n)])
	d.pos += int(n)
	return s, nil
}

func (d *decoder) readRemaining() []byte {
	b := d.buf[d.pos:]
	d.pos = len(d.buf)
	return b
}

// --- packet builders ---

// encodeConnect builds a CONNECT packet (socket.ts encodeConnectPacket).
// Field order: version(1) deviceFlag(1) deviceID(str) uid(str) token(str)
// clientTimestamp(int64) clientKey(str).
func encodeConnect(deviceID, uid, token string, clientTimestampMS uint64, clientKeyB64 string) []byte {
	var b encoder
	b.writeByte(protoVersion)
	b.writeByte(0) // deviceFlag = app/bot
	b.writeString(deviceID)
	b.writeString(uid)
	b.writeString(token)
	b.writeInt64(clientTimestampMS)
	b.writeString(clientKeyB64)
	return frame(pktConnect, b.buf)
}

// encodePing is a single header byte (no body, no varint).
func encodePing() []byte { return []byte{byte(pktPing)<<4 | 0} }

// encodeRecvack builds a RECVACK (socket.ts encodeRecvackPacket): int64
// messageID + int32 messageSeq.
func encodeRecvack(messageID uint64, messageSeq uint32) []byte {
	var b encoder
	b.writeInt64(messageID)
	b.writeInt32(messageSeq)
	return frame(pktRecvack, b.buf)
}

// --- frame splitting ---

// nextFrame attempts to split one full frame from buf. Returns the packet type,
// the body (after header+varint), the total bytes consumed, and ok=false if a
// complete frame isn't buffered yet.
func nextFrame(buf []byte) (pt packetType, body []byte, consumed int, ok bool, err error) {
	if len(buf) < 1 {
		return 0, nil, 0, false, nil
	}
	header := buf[0]
	pt = packetType(header >> 4)

	// PING/PONG are single-byte packets with no length/body.
	if pt == pktPing || pt == pktPong {
		return pt, nil, 1, true, nil
	}

	// Decode remaining-length varint starting at byte 1.
	remLen := 0
	mult := 1
	i := 1
	for {
		if i > len(buf)-1 {
			return 0, nil, 0, false, nil // incomplete varint
		}
		if i-1 >= maxVarlenBytes {
			return 0, nil, 0, false, fmt.Errorf("octo: varint too long")
		}
		digit := buf[i]
		i++
		remLen += int(digit&127) * mult
		mult *= 128
		if digit&0x80 == 0 {
			break
		}
	}
	total := i + remLen
	if total > len(buf) {
		return 0, nil, 0, false, nil // body not fully buffered
	}
	return pt, buf[i:total], total, true, nil
}

// flags extracts the low-nibble flags from a header byte (for RECV/CONNACK).
func headerFlags(header byte) byte { return header & 0x0F }
