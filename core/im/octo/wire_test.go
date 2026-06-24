package octo

import (
	"bytes"
	"errors"
	"testing"
)

func TestVarintEncode(t *testing.T) {
	cases := map[int][]byte{
		0:     {0x00},
		127:   {0x7f},
		128:   {0x80, 0x01},
		16384: {0x80, 0x80, 0x01},
	}
	for n, want := range cases {
		if got := encodeVariableLength(n); !bytes.Equal(got, want) {
			t.Fatalf("varint(%d)=%v want %v", n, got, want)
		}
	}
}

func TestStringRoundTrip(t *testing.T) {
	var e encoder
	e.writeString("hello 世界")
	e.writeInt64(0x0102030405060708)
	d := &decoder{buf: e.buf}
	s, err := d.readString()
	if err != nil || s != "hello 世界" {
		t.Fatalf("string rt: %q %v", s, err)
	}
	v, err := d.readInt64()
	if err != nil || v != 0x0102030405060708 {
		t.Fatalf("int64 rt: %x %v", v, err)
	}
}

func TestFrameHeaderAndSplit(t *testing.T) {
	// Build a CONNECT and confirm header nibble + varint + body split back out.
	pkt := encodeConnect("devW", "robot1", "tok1", 1700000000000, "pubkeyb64")
	if pkt[0]>>4 != byte(pktConnect) {
		t.Fatalf("header packet type = %d", pkt[0]>>4)
	}
	body := assertConnectFrameSplit(t, pkt)
	assertConnectBody(t, body)
}

func assertConnectFrameSplit(t *testing.T, pkt []byte) []byte {
	t.Helper()

	pt, body, consumed, ok, err := nextFrame(pkt)
	if err != nil || !ok {
		t.Fatalf("nextFrame: ok=%v err=%v", ok, err)
	}
	if pt != pktConnect {
		t.Fatalf("pt=%d", pt)
	}
	if consumed != len(pkt) {
		t.Fatalf("consumed %d != %d", consumed, len(pkt))
	}
	return body
}

func assertConnectBody(t *testing.T, body []byte) {
	t.Helper()

	d := &decoder{buf: body}
	ver, _ := d.readByte()
	flag, _ := d.readByte()
	dev, _ := d.readString()
	uid, _ := d.readString()
	tok, _ := d.readString()
	ts, _ := d.readInt64()
	ck, _ := d.readString()
	if ver != protoVersion || flag != 0 || dev != "devW" || uid != "robot1" || tok != "tok1" || ts != 1700000000000 || ck != "pubkeyb64" {
		t.Fatalf("connect body wrong: ver=%d flag=%d dev=%q uid=%q tok=%q ts=%d ck=%q", ver, flag, dev, uid, tok, ts, ck)
	}
}

func TestPingSingleByte(t *testing.T) {
	p := encodePing()
	if len(p) != 1 || p[0]>>4 != byte(pktPing) {
		t.Fatalf("ping = %v", p)
	}
	pt, _, consumed, ok, err := nextFrame(p)
	if err != nil || !ok || pt != pktPing || consumed != 1 {
		t.Fatalf("ping frame: pt=%d consumed=%d ok=%v err=%v", pt, consumed, ok, err)
	}
}

func TestNextFrameIncomplete(t *testing.T) {
	pkt := encodeConnect("d", "u", "t", 1, "k")
	// Feed all but the last byte: should report not-ok (wait for more).
	_, _, _, ok, err := nextFrame(pkt[:len(pkt)-1])
	if err != nil || ok {
		t.Fatalf("partial frame should be incomplete: ok=%v err=%v", ok, err)
	}
}

func TestRecvackFrame(t *testing.T) {
	pkt := encodeRecvack(0x1122334455667788, 0x09)
	pt, body, _, ok, _ := nextFrame(pkt)
	if !ok || pt != pktRecvack {
		t.Fatalf("recvack pt=%d ok=%v", pt, ok)
	}
	d := &decoder{buf: body}
	id, _ := d.readInt64()
	seq, _ := d.readInt32()
	if id != 0x1122334455667788 || seq != 0x09 {
		t.Fatalf("recvack body: id=%x seq=%d", id, seq)
	}
}

// TestNextFrameSentinels exercises the exported error sentinels so callers
// (and any downstream metric scraper) can rely on errors.Is matching them.
// This is the regression for the commit that promoted the prior
// fmt.Errorf strings to exported sentinels — exported API surface without
// an Is-consumer is a contract claim without a test.
func TestNextFrameSentinels(t *testing.T) {
	t.Run("unknown packet type", func(t *testing.T) {
		// Header byte with packet type 0 (below pktConnect=1) — must reject.
		_, _, _, _, err := nextFrame([]byte{0x00})
		if !errors.Is(err, ErrUnknownPacketType) {
			t.Fatalf("want ErrUnknownPacketType, got %v", err)
		}
	})
	t.Run("varint too long", func(t *testing.T) {
		// pktSend header + 5 continuation bytes (one over maxVarlenBytes=4).
		pkt := []byte{byte(pktSend) << 4, 0x80, 0x80, 0x80, 0x80, 0x80}
		_, _, _, _, err := nextFrame(pkt)
		if !errors.Is(err, ErrVarintTooLong) {
			t.Fatalf("want ErrVarintTooLong, got %v", err)
		}
	})
	t.Run("frame body too large", func(t *testing.T) {
		// pktSend header + varint encoding maxFrameBodyBytes+1.
		body := []byte{byte(pktSend) << 4}
		body = append(body, encodeVariableLength(maxFrameBodyBytes+1)...)
		_, _, _, _, err := nextFrame(body)
		if !errors.Is(err, ErrFrameBodyTooLarge) {
			t.Fatalf("want ErrFrameBodyTooLarge, got %v", err)
		}
	})
}
