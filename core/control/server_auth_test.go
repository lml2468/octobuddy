package control

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"testing"
	"time"
)

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

// TestCapabilityTokenGate is the regression: privileged commands require
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

	assertUnauthenticatedSecretRejected(t, conn, sc)
	assertHealthAllowedWithoutAuth(t, conn, sc)
	assertWrongTokenRejected(t, conn, sc)
	resp := assertValidTokenAllowsSecret(t, conn, sc, token)
	assertTokenNotReflected(t, resp, token)
}

func isErrorEnvelope(env Envelope) bool {
	return env.Type == "error"
}

func assertUnauthenticatedSecretRejected(t *testing.T, conn net.Conn, sc *bufio.Scanner) {
	t.Helper()
	if env := sendCmd(t, conn, sc, "1", "secret.inject", SecretInjectBody{Kind: "octoToken", Value: "x"}); !isErrorEnvelope(env) {
		t.Fatalf("unauthenticated secret.inject should be rejected, got %+v", env)
	}
}

func assertHealthAllowedWithoutAuth(t *testing.T, conn net.Conn, sc *bufio.Scanner) {
	t.Helper()
	if env := sendCmd(t, conn, sc, "2", "health", struct{}{}); isErrorEnvelope(env) {
		t.Fatalf("health should work without auth, got error %+v", env)
	}
}

func assertWrongTokenRejected(t *testing.T, conn net.Conn, sc *bufio.Scanner) {
	t.Helper()

	const wrong = "wrong-token-zzz"
	env := sendCmd(t, conn, sc, "3", CmdAuth, AuthBody{Token: wrong})
	if !isErrorEnvelope(env) {
		t.Fatalf("auth with wrong token should be rejected, got %+v", env)
	}
	if bytes.Contains(env.Body, []byte(wrong)) {
		t.Fatalf("rejection echoed the presented token: %s", env.Body)
	}
	if env := sendCmd(t, conn, sc, "4", "secret.inject", SecretInjectBody{Kind: "octoToken", Value: "x"}); !isErrorEnvelope(env) {
		t.Fatalf("secret.inject after failed auth should be rejected, got %+v", env)
	}
}

func assertValidTokenAllowsSecret(t *testing.T, conn net.Conn, sc *bufio.Scanner, token string) Envelope {
	t.Helper()

	if env := sendCmd(t, conn, sc, "5", CmdAuth, AuthBody{Token: token}); isErrorEnvelope(env) {
		t.Fatalf("auth with valid token should succeed, got error %+v", env)
	}
	resp := sendCmd(t, conn, sc, "6", "secret.inject", SecretInjectBody{Kind: "octoToken", Value: "x"})
	if isErrorEnvelope(resp) {
		t.Fatalf("secret.inject after valid auth should succeed, got error %+v", resp)
	}
	return resp
}

func assertTokenNotReflected(t *testing.T, resp Envelope, token string) {
	t.Helper()
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
