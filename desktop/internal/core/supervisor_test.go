package core

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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

// TestTokenDeliveredOutOfBand is the supervisor regression: the minted
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

// TestEnvWithOctoBinAugmentsPath verifies the daemon env carries a PATH that
// (1) leads with ~/.xclaw/bin, (2) includes the well-known agent install dirs so
// a GUI-launched daemon finds `claude`, and (3) still contains the inherited
// PATH entries, with no duplicates.
func TestEnvWithOctoBinAugmentsPath(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "inherited-bin")
	t.Setenv("PATH", sentinel)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	sep := string(os.PathListSeparator)

	var path string
	for _, kv := range envWithOctoBin() {
		if strings.HasPrefix(kv, "PATH=") {
			path = strings.TrimPrefix(kv, "PATH=")
		}
	}
	if path == "" {
		t.Fatal("envWithOctoBin produced no PATH")
	}
	entries := strings.Split(path, sep)

	octoBin := filepath.Join(home, ".xclaw", "bin")
	if entries[0] != octoBin {
		t.Fatalf("PATH does not lead with %q: %q", octoBin, entries[0])
	}
	if !slices.Contains(entries, sentinel) {
		t.Fatalf("inherited PATH entry %q dropped: %v", sentinel, entries)
	}
	for _, d := range wellKnownBinDirs(home) {
		if !slices.Contains(entries, d) {
			t.Fatalf("well-known dir %q missing from PATH: %v", d, entries)
		}
	}

	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e] {
			t.Fatalf("duplicate PATH entry %q: %v", e, entries)
		}
		seen[e] = true
	}
}

// TestLoginShellPathFencing exercises the marker-fenced login-shell probe with a
// fake $SHELL that prints a banner around the value. Unix only (no $SHELL probe
// on Windows).
func TestLoginShellPathFencing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no login-shell PATH probe on Windows")
	}
	// A fake shell script: emits noise on stderr + stdout, sets a sentinel PATH,
	// then runs the command our code passes as the 3rd arg (after -l -c), which
	// prints the marker-fenced PATH to stdout.
	dir := t.TempDir()
	fake := filepath.Join(dir, "fakeshell.sh")
	script := "#!/bin/sh\n" +
		"echo 'MOTD banner' >&2\n" + // stderr noise: never captured
		"echo 'login greeting'\n" + // stdout noise BEFORE the marker: stripped by fencing
		"PATH='/fake/agent/bin:/usr/bin'\n" +
		"eval \"$3\"\n" // argv is (-l, -c, CMD) → $3 is the printf command
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake shell: %v", err)
	}
	t.Setenv("SHELL", fake)

	got := loginShellPath()
	if got != "/fake/agent/bin:/usr/bin" {
		t.Fatalf("loginShellPath() = %q, want %q", got, "/fake/agent/bin:/usr/bin")
	}
}
