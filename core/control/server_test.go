package control

import (
	"encoding/json"
	"net"
	"testing"
	"time"
)

// TestServerCommandResponse drives the server over an in-memory pipe: send a
// command, get a correlated response.
func TestServerCommandResponse(t *testing.T) {
	srv := NewServer(func(cmdType string, body json.RawMessage) (any, error) {
		if cmdType == "health" {
			return HealthBody{Driver: "fake", Uptime: 42}, nil
		}
		return OKBody{OK: true}, nil
	})

	ln := newPipeListener()
	go srv.Serve(ln)
	defer srv.Close()

	clientConn := ln.dial()
	defer clientConn.Close()

	// send a command
	body, _ := json.Marshal(struct{}{})
	line, _ := Encode(Envelope{Kind: KindCommand, ID: "h1", Type: "health", Body: body})
	if _, err := clientConn.Write(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	// read the response
	sc := NewScanner(clientConn)
	if !sc.Scan() {
		t.Fatalf("no response: %v", sc.Err())
	}
	env, err := Decode(sc.Bytes())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Kind != KindResponse || env.ID != "h1" {
		t.Fatalf("bad response envelope: %+v", env)
	}
	var hb HealthBody
	if err := json.Unmarshal(env.Body, &hb); err != nil || hb.Driver != "fake" {
		t.Fatalf("bad health body: %+v %v", hb, err)
	}
}

// TestServerBroadcast verifies events reach a connected client (the path the
// gateway EventSink uses). net.Pipe is unbuffered, so the read must be in flight
// before/while the server writes — we start the reader goroutine first.
func TestServerBroadcast(t *testing.T) {
	srv := NewServer(nil)
	ln := newPipeListener()
	go srv.Serve(ln)
	defer srv.Close()

	clientConn := ln.dial()
	defer clientConn.Close()

	// wait until the server has registered the connection
	deadline := time.Now().Add(time.Second)
	for srv.ConnCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	// Read concurrently so the unbuffered pipe write can complete.
	got := make(chan Envelope, 1)
	go func() {
		sc := NewScanner(clientConn)
		if sc.Scan() {
			env, _ := Decode(sc.Bytes())
			got <- env
		} else {
			close(got)
		}
	}()

	srv.Broadcast("session.text", SessionTextBody{SessionKey: "u1", Delta: "hello"})

	select {
	case env, ok := <-got:
		if !ok {
			t.Fatal("scan failed before any event")
		}
		if env.Kind != KindEvent || env.Type != "session.text" {
			t.Fatalf("bad event: %+v", env)
		}
		var tb SessionTextBody
		_ = json.Unmarshal(env.Body, &tb)
		if tb.Delta != "hello" {
			t.Fatalf("bad text body: %+v", tb)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for broadcast event")
	}
}

// --- in-memory listener over net.Pipe ---

// TestBroadcastDuringDisconnect stresses the path that used to crash the daemon:
// a client disconnecting (which closes its send channel) while Broadcast is
// mid-fan-out. With sendCh closed by close, enqueue's `c.sendCh <- line` would
// panic "send on closed channel" (select/default does NOT catch a closed send).
// The fix stops the write loop via a separate done channel and never closes
// sendCh, so this must run clean (a panic in the enqueue goroutine would crash
// the test binary).
func TestBroadcastDuringDisconnect(t *testing.T) {
	srv := NewServer(nil)
	ln := newPipeListener()
	go srv.Serve(ln)
	defer srv.Close()

	done := make(chan struct{})
	// Broadcaster: spam events continuously.
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				srv.Broadcast("session.text", SessionTextBody{SessionKey: "u", Delta: "x"})
			}
		}
	}()

	// Churn clients: connect then immediately close, racing the broadcaster's
	// snapshot→enqueue window against client.close.
	for i := 0; i < 200; i++ {
		c := ln.dial()
		for srv.ConnCount() == 0 {
			time.Sleep(50 * time.Microsecond)
		}
		c.Close()
		for srv.ConnCount() != 0 {
			time.Sleep(50 * time.Microsecond)
		}
	}
	close(done)
}

type pipeListener struct {
	conns chan net.Conn
}

func newPipeListener() *pipeListener { return &pipeListener{conns: make(chan net.Conn, 4)} }

func (p *pipeListener) Accept() (net.Conn, error) {
	c, ok := <-p.conns
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (p *pipeListener) Close() error   { return nil }
func (p *pipeListener) Addr() net.Addr { return pipeAddr{} }

func (p *pipeListener) dial() net.Conn {
	server, client := net.Pipe()
	p.conns <- server
	return client
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }
