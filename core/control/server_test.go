package control

import (
	"bufio"
	"bytes"
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
// mid-fan-out. With sendCh closed by close(), enqueue's `c.sendCh <- line` would
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
	// snapshot→enqueue window against client.close().
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

// sendCmd writes one command and returns the next decoded frame (its response).
// Each connection processes commands FIFO, so the next frame after a command is
// that command's response (no events are broadcast on these connections).
func sendCmd(t *testing.T, conn net.Conn, sc *bufio.Scanner, id, typ string, body any) Envelope {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal %s body: %v", typ, err)
	}
	line, _ := Encode(Envelope{Kind: KindCommand, ID: id, Type: typ, Body: raw})
	if _, err := conn.Write(line); err != nil {
		t.Fatalf("write %s: %v", typ, err)
	}
	if !sc.Scan() {
		t.Fatalf("no response to %s: %v", typ, sc.Err())
	}
	env, err := Decode(sc.Bytes())
	if err != nil {
		t.Fatalf("decode %s response: %v", typ, err)
	}
	return env
}

// TestCapabilityTokenGate is the MLT-37 regression: privileged commands require
// the GUI capability token; read-only commands do not. It exercises the four
// required cases — (i) privileged without/with a wrong token is rejected, (ii)
// privileged with the valid token is accepted, (iii) a read-only command works
// without any token, and (iv) the token is never echoed back to the client.
func TestCapabilityTokenGate(t *testing.T) {
	const token = "correct-horse-battery-staple"
	srv := NewServer(func(cmdType string, body json.RawMessage) (any, error) {
		return OKBody{OK: true}, nil // any reached command "succeeds"
	})
	srv.SetAuth(token, []string{"secret.inject"})

	ln := newPipeListener()
	go srv.Serve(ln)
	defer srv.Close()

	conn := ln.dial()
	defer conn.Close()
	sc := NewScanner(conn)

	isErr := func(env Envelope) bool { return env.Type == "error" }

	// (i) privileged command before any auth → rejected.
	if env := sendCmd(t, conn, sc, "1", "secret.inject", SecretInjectBody{Kind: "octoToken", Value: "x"}); !isErr(env) {
		t.Fatalf("unauthenticated secret.inject should be rejected, got %+v", env)
	}

	// (iii) read-only command works without auth.
	if env := sendCmd(t, conn, sc, "2", "health", struct{}{}); isErr(env) {
		t.Fatalf("health should work without auth, got error %+v", env)
	}

	// (i) auth with the WRONG token → rejected, and the wrong token must not be
	// echoed back (no reflection into the response/logs).
	const wrong = "wrong-token-zzz"
	env := sendCmd(t, conn, sc, "3", CmdAuth, AuthBody{Token: wrong})
	if !isErr(env) {
		t.Fatalf("auth with wrong token should be rejected, got %+v", env)
	}
	if bytes.Contains(env.Body, []byte(wrong)) {
		t.Fatalf("rejection echoed the presented token: %s", env.Body)
	}
	// still unauthenticated → privileged still rejected.
	if env := sendCmd(t, conn, sc, "4", "secret.inject", SecretInjectBody{Kind: "octoToken", Value: "x"}); !isErr(env) {
		t.Fatalf("secret.inject after failed auth should be rejected, got %+v", env)
	}

	// (ii) auth with the VALID token → accepted, then privileged succeeds.
	if env := sendCmd(t, conn, sc, "5", CmdAuth, AuthBody{Token: token}); isErr(env) {
		t.Fatalf("auth with valid token should succeed, got error %+v", env)
	}
	resp := sendCmd(t, conn, sc, "6", "secret.inject", SecretInjectBody{Kind: "octoToken", Value: "x"})
	if isErr(resp) {
		t.Fatalf("secret.inject after valid auth should succeed, got error %+v", resp)
	}

	// (iv) the valid token must never be reflected to the client across any frame.
	if bytes.Contains(resp.Body, []byte(token)) {
		t.Fatalf("response leaked the capability token: %s", resp.Body)
	}
}

// TestCapabilityTokenFailClosed verifies that with no token configured (empty),
// no presented token can authenticate, so every privileged command is denied —
// the fail-closed default a bare CLI/dev daemon falls back to.
func TestCapabilityTokenFailClosed(t *testing.T) {
	srv := NewServer(func(string, json.RawMessage) (any, error) { return OKBody{OK: true}, nil })
	srv.SetAuth("", []string{"secret.inject"}) // empty token → unauthenticatable

	ln := newPipeListener()
	go srv.Serve(ln)
	defer srv.Close()
	conn := ln.dial()
	defer conn.Close()
	sc := NewScanner(conn)

	// Even an "auth" with some token cannot authenticate against an empty config.
	if env := sendCmd(t, conn, sc, "1", CmdAuth, AuthBody{Token: "anything"}); env.Type != "error" {
		t.Fatalf("auth against empty token should be rejected, got %+v", env)
	}
	if env := sendCmd(t, conn, sc, "2", "secret.inject", SecretInjectBody{Kind: "octoToken", Value: "x"}); env.Type != "error" {
		t.Fatalf("privileged command must be denied when no token is configured, got %+v", env)
	}
}

// TestBroadcastDeniedToUnauthed verifies the cross-session event stream is
// operator-only when a token is configured: an unauthenticated connection
// receives no broadcast events. (With no token, TestServerBroadcast covers the
// open path.)
func TestBroadcastDeniedToUnauthed(t *testing.T) {
	srv := NewServer(nil)
	srv.SetAuth("tok", []string{"secret.inject"})
	ln := newPipeListener()
	go srv.Serve(ln)
	defer srv.Close()

	conn := ln.dial()
	defer conn.Close()
	waitConn(srv)

	got := make(chan string, 1)
	go func() {
		sc := NewScanner(conn)
		if sc.Scan() {
			env, _ := Decode(sc.Bytes())
			got <- env.Type
		}
	}()
	srv.Broadcast("session.text", SessionTextBody{Delta: "secret-x"})
	select {
	case ev := <-got:
		t.Fatalf("unauthenticated client received gated event %q", ev)
	case <-time.After(150 * time.Millisecond):
		// expected: nothing delivered to an unauthenticated peer
	}
}

// TestBroadcastDeliveredToAuthed verifies an authenticated connection still
// receives the broadcast event stream once a token is configured.
func TestBroadcastDeliveredToAuthed(t *testing.T) {
	srv := NewServer(nil)
	srv.SetAuth("tok", []string{"secret.inject"})
	ln := newPipeListener()
	go srv.Serve(ln)
	defer srv.Close()

	conn := ln.dial()
	defer conn.Close()
	sc := NewScanner(conn)
	waitConn(srv)

	// Authenticate synchronously (consume the auth response), then expect events.
	if env := sendCmd(t, conn, sc, "1", CmdAuth, AuthBody{Token: "tok"}); env.Type == "error" {
		t.Fatalf("auth should succeed: %+v", env)
	}

	got := make(chan string, 1)
	go func() {
		if sc.Scan() {
			env, _ := Decode(sc.Bytes())
			got <- env.Type
		}
	}()
	srv.Broadcast("session.text", SessionTextBody{Delta: "hello"})
	select {
	case ev := <-got:
		if ev != "session.text" {
			t.Fatalf("got unexpected event %q", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("authenticated client did not receive broadcast event")
	}
}

func waitConn(srv *Server) {
	deadline := time.Now().Add(time.Second)
	for srv.ConnCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
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
