package core

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestMain doubles as a fake xclawd: when XCLAW_FAKE_DAEMON=1 the test binary
// impersonates the daemon so TestTokenDeliveredOutOfBand can inspect exactly
// what the supervisor handed it (argv, environ, stdin).
func TestMain(m *testing.M) {
	if os.Getenv("XCLAW_FAKE_DAEMON") == "1" {
		fakeDaemon()
		return
	}
	os.Exit(m.Run())
}

// fakeDaemon mimics the bits of xclawd the supervisor relies on: it reads the
// capability token from stdin (the out-of-band channel), creates the control
// socket file so the supervisor's startup wait succeeds, and records what it saw
// (argv, environ, the stdin token) to <socket>.seen for the test to assert on.
func fakeDaemon() {
	var sock string
	for i, a := range os.Args {
		if a == "-control" && i+1 < len(os.Args) {
			sock = os.Args[i+1]
		}
	}
	tokenLine, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	seen := map[string]any{
		"argv":       os.Args,
		"environ":    os.Environ(),
		"stdinToken": strings.TrimSpace(tokenLine),
	}
	b, _ := json.Marshal(seen)
	_ = os.WriteFile(sock+".seen", b, 0o600)
	_ = os.WriteFile(sock, nil, 0o600) // satisfy the supervisor's socket-exists wait
	os.Exit(0)
}

// TestTokenDeliveredOutOfBand is the MLT-37 supervisor regression: the minted
// capability token must reach the daemon on stdin and must NOT appear in its
// argv or environment (both world-readable via /proc on Linux).
func TestTokenDeliveredOutOfBand(t *testing.T) {
	t.Setenv("XCLAW_FAKE_DAEMON", "1")
	sock := filepath.Join(t.TempDir(), "x.sock")

	sup := &Supervisor{BinPath: os.Args[0], SocketPath: sock}
	if err := sup.Start(); err != nil {
		t.Fatalf("start fake daemon: %v", err)
	}
	defer sup.Stop()

	token := sup.Token()
	if len(token) != 64 { // 32 random bytes, hex-encoded
		t.Fatalf("token = %q, want 64 hex chars", token)
	}

	raw, err := os.ReadFile(sock + ".seen")
	if err != nil {
		t.Fatalf("read seen file: %v", err)
	}
	var seen struct {
		Argv       []string `json:"argv"`
		Environ    []string `json:"environ"`
		StdinToken string   `json:"stdinToken"`
	}
	if err := json.Unmarshal(raw, &seen); err != nil {
		t.Fatalf("unmarshal seen: %v", err)
	}

	if seen.StdinToken != token {
		t.Fatalf("stdin token = %q, want %q", seen.StdinToken, token)
	}
	if slices.Contains(seen.Argv, token) {
		t.Fatalf("token leaked into argv: %v", seen.Argv)
	}
	if !slices.Contains(seen.Argv, "-control-auth-stdin") {
		t.Fatalf("daemon not told to read the token from stdin: %v", seen.Argv)
	}
	for _, kv := range seen.Environ {
		if strings.Contains(kv, token) {
			t.Fatalf("token leaked into environ: %q", kv)
		}
	}
}

// TestRandomTokenUnique verifies minted tokens are distinct and well-formed.
func TestRandomTokenUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := randomToken()
		if err != nil {
			t.Fatalf("randomToken: %v", err)
		}
		if len(tok) != 64 {
			t.Fatalf("token len = %d, want 64", len(tok))
		}
		if seen[tok] {
			t.Fatalf("duplicate token: %s", tok)
		}
		seen[tok] = true
	}
}

// TestDaemonArgsOmitToken guards the invariant that the token is never an argv
// element — the whole point of the stdin channel.
func TestDaemonArgsOmitToken(t *testing.T) {
	args := daemonArgs("/tmp/x.sock", "/home/u/.xclaw/config.json")
	if !slices.Contains(args, "-control-auth-stdin") {
		t.Fatalf("missing -control-auth-stdin: %v", args)
	}
	if !slices.Contains(args, "-config") {
		t.Fatalf("config mode args missing -config: %v", args)
	}
}
