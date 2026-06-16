package main

import (
	"io"
	"strings"
	"testing"
)

// TestReadControlToken covers the capability-token reader the daemon uses to
// pull the GUI token off its stdin (MLT-37): a newline-terminated line, a
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
// MLT-37 threat surface (the GUI→daemon operations an injected same-uid agent
// must not reach) and does NOT gate read-only commands.
func TestPrivilegedControlCommandsCoverThreat(t *testing.T) {
	priv := map[string]bool{}
	for _, c := range privilegedControlCommands {
		priv[c] = true
	}
	for _, want := range []string{"session.send", "session.reset", "secret.inject", "cron.create", "cron.delete"} {
		if !priv[want] {
			t.Errorf("expected %q to be privileged", want)
		}
	}
	for _, open := range []string{"health", "bots.list", "session.history", "cron.list"} {
		if priv[open] {
			t.Errorf("read-only %q must NOT be gated", open)
		}
	}
}
