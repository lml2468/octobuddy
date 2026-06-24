package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/lml2468/octobuddy/core/control"
)

// TestReadControlToken covers the capability-token reader the daemon uses to
// pull the GUI token off its stdin: a newline-terminated line, a
// close-without-newline (EOF), surrounding whitespace, and an empty stream.
func TestReadControlToken(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"newline terminated", "deadbeef\n", "deadbeef"},
		{"eof no newline", "deadbeef", "deadbeef"},
		{"trims whitespace", "  deadbeef \n", "deadbeef"},
		{"ignores trailing lines", "tok\nLATER LOG LINE\n", "tok"},
		{"empty", "", ""},
		{"blank line", "\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readControlToken(strings.NewReader(tc.in))
			if err != nil {
				t.Fatalf("readControlToken(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("readControlToken(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestReadControlTokenBounded ensures the reader cannot be made to slurp an
// unbounded stream into memory — it stops at maxTokenBytes even with no newline.
func TestReadControlTokenBounded(t *testing.T) {
	got, err := readControlToken(io.LimitReader(neverEnding{}, 1<<20))
	if err != nil {
		t.Fatalf("readControlToken bounded: %v", err)
	}
	if len(got) > maxTokenBytes {
		t.Fatalf("read %d bytes, want <= %d", len(got), maxTokenBytes)
	}
}

// neverEnding yields an endless stream of non-newline bytes.
type neverEnding struct{}

func (neverEnding) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

// TestPrivilegedControlCommandsCoverThreat asserts the gated set matches the
// threat surface (the GUI→daemon operations an injected same-uid agent
// must not reach) and does NOT gate the open metadata commands. session.history
// and cron.list are gated: their handlers take an attacker-controllable
// botId/sessionKey with no scoping, so they are the at-rest twin of the gated
// cross-session event stream.
func TestPrivilegedControlCommandsCoverThreat(t *testing.T) {
	priv := map[string]bool{}
	for _, c := range privilegedControlCommands {
		priv[c] = true
	}
	for _, want := range []string{
		"session.send", "session.reset", "secret.inject",
		"session.history", "sessions.list", "usage.stats", "cron.create", "cron.list", "cron.delete",
	} {
		if !priv[want] {
			t.Errorf("expected %q to be privileged", want)
		}
	}
	for _, open := range []string{"health", "bots.list"} {
		if priv[open] {
			t.Errorf("metadata command %q must NOT be gated", open)
		}
	}
}

// TestSessionHistoryGatedEndToEnd is the regression: wired with the REAL
// privilegedControlCommands, an unauthenticated connection (the same-uid spawned
// agent) cannot read session.history or cron.list, and the handler is never even
// reached — the gate rejects before dispatch. After presenting the valid token
// (what the GUI does first on its FIFO connection), the same commands dispatch.
// This locks the at-rest cross-session disclosure the broadcast stream already
// gates: if session.history/cron.list ever drift back to the open set, this fails.
func TestSessionHistoryGatedEndToEnd(t *testing.T) {
	const token = "cap-token-1234"
	handlerHits := 0
	srv := control.NewServer(func(cmdType string, body json.RawMessage) (any, error) {
		handlerHits++
		return control.OKBody{OK: true}, nil
	})
	srv.SetAuth(token, privilegedControlCommands)

	ln := newPipeListener()
	go srv.Serve(ln)
	defer srv.Close()

	conn := ln.dial()
	defer conn.Close()
	sc := control.NewScanner(conn)

	// Unauthenticated: both privileged reads are refused before dispatch.
	if env := roundTripControl(t, conn, sc, "h1", "session.history", control.SessionHistoryBody{BotID: "b1", SessionKey: "victim", Limit: 1000}); !isUnauthorized(env) {
		t.Fatalf("unauthenticated session.history must be rejected, got %+v", env)
	}
	if env := roundTripControl(t, conn, sc, "c1", "cron.list", control.CronListBody{BotID: "b1"}); !isUnauthorized(env) {
		t.Fatalf("unauthenticated cron.list must be rejected, got %+v", env)
	}
	if handlerHits != 0 {
		t.Fatalf("handler ran for a gated command before auth (%d hits)", handlerHits)
	}

	// Authenticate (the GUI's first send), then the same command dispatches.
	if env := roundTripControl(t, conn, sc, "a1", control.CmdAuth, control.AuthBody{Token: token}); env.Type == "error" {
		t.Fatalf("auth with valid token should succeed, got %+v", env)
	}
	if env := roundTripControl(t, conn, sc, "h2", "session.history", control.SessionHistoryBody{BotID: "b1", SessionKey: "self", Limit: 40}); env.Type == "error" {
		t.Fatalf("authenticated session.history should dispatch, got %+v", env)
	}
	if handlerHits != 1 {
		t.Fatalf("expected exactly one handler hit after auth, got %d", handlerHits)
	}
}

func roundTripControl(t *testing.T, conn net.Conn, sc *bufio.Scanner, id, cmdType string, body any) control.Envelope {
	t.Helper()
	raw, _ := json.Marshal(body)
	line, _ := control.Encode(control.Envelope{Kind: control.KindCommand, ID: id, Type: cmdType, Body: raw})
	if _, err := conn.Write(line); err != nil {
		t.Fatalf("write %s: %v", cmdType, err)
	}
	if !sc.Scan() {
		t.Fatalf("no response to %s: %v", cmdType, sc.Err())
	}
	env, err := control.Decode(sc.Bytes())
	if err != nil {
		t.Fatalf("decode %s response: %v", cmdType, err)
	}
	return env
}

func isUnauthorized(env control.Envelope) bool {
	if env.Type != "error" {
		return false
	}
	var eb control.ErrorBody
	_ = json.Unmarshal(env.Body, &eb)
	return strings.Contains(eb.Message, "unauthorized")
}

// pipeListener is an in-memory net.Listener over net.Pipe, mirroring the helper
// in core/control's server tests — it lets this package drive a real
// control.Server without binding a unix socket.
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
