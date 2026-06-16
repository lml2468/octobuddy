// Package core supervises the xclawd daemon subprocess for the desktop app:
// it resolves the binary, picks a control-socket path, spawns the daemon in
// control-bus mode (tied to the app's lifetime via -exit-with-parent), and
// stops it cleanly. Reconnect/backoff policy is layered on in Phase 1.
package core

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// envWithOctoBin returns the process environment with ~/.xclaw/bin prepended to
// PATH, so the daemon's spawned agent can invoke the octo-cli companion binary.
func envWithOctoBin() []string {
	env := os.Environ()
	home, err := os.UserHomeDir()
	if err != nil {
		return env
	}
	bin := filepath.Join(home, ".xclaw", "bin")
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=" + bin + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PATH=")
			return env
		}
	}
	return append(env, "PATH="+bin)
}

// SocketPath returns the control-bus socket path for this user. Kept short to
// stay under the ~104-byte sockaddr_un limit. On Windows, AF_UNIX still wants a
// filesystem path (Win10 1803+), placed in the temp dir.
func SocketPath() string {
	name := fmt.Sprintf("xclaw-%d.sock", os.Getuid())
	if runtime.GOOS == "windows" {
		return filepath.Join(os.TempDir(), name)
	}
	return filepath.Join("/tmp", name)
}

// ConfigPath is the daemon's multi-bot config (~/.xclaw/config.json).
func ConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw", "config.json")
}

// ResolveBinary finds the xclawd executable, mirroring the Swift CorePaths
// order: explicit override → bundled helper → monorepo dev build → PATH.
func ResolveBinary() (string, error) {
	if override := os.Getenv("XCLAWD_BIN"); override != "" {
		if isExec(override) {
			return override, nil
		}
	}
	// Bundled next to the app executable (production): ../Helpers/xclawd.
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "..", "Helpers", binName())
		if isExec(cand) {
			return filepath.Clean(cand), nil
		}
	}
	// Monorepo dev: walk up from cwd looking for core/.xclawd-dev.
	if dir, err := os.Getwd(); err == nil {
		for i := 0; i < 6; i++ {
			cand := filepath.Join(dir, "core", ".xclawd-dev")
			if isExec(cand) {
				return cand, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	// PATH fallback.
	if p, err := exec.LookPath(binName()); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("xclawd binary not found (set XCLAWD_BIN, build core/.xclawd-dev, or install xclawd)")
}

// Supervisor owns one xclawd process. All lifecycle methods (Start/Stop/Restart)
// are serialized by mu so the cmd field is never read/written concurrently —
// the desktop calls these from both the UI thread (RestartCore) and the
// daemon-read goroutine (crash reconnect), which would otherwise race on cmd
// and could spawn two daemons fighting over the socket.
type Supervisor struct {
	BinPath    string
	SocketPath string
	ConfigPath string // when non-empty, run -config mode

	mu        sync.Mutex
	cmd       *exec.Cmd
	authToken string // capability token minted for the current daemon (MLT-37)
}

// Token returns the control-bus capability token for the currently running
// daemon. The GUI presents it in an "auth" handshake right after dialing so the
// daemon admits its privileged commands (cron.*, secret.inject, session.*). A
// fresh token is minted on every Start/Restart, so callers must read it after a
// (re)connect, never cache it across daemon restarts. Empty before the first
// successful Start.
func (s *Supervisor) Token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authToken
}

// Start spawns xclawd in control-bus mode and waits for the socket to appear.
func (s *Supervisor) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startLocked()
}

func (s *Supervisor) startLocked() error {
	args := daemonArgs(s.SocketPath, s.ConfigPath)
	// A stale socket from a crashed prior run would make the daemon's listen fail.
	_ = os.Remove(s.SocketPath)

	// Mint a fresh capability token and hand it to the daemon out-of-band: over a
	// private pipe wired to the daemon's stdin (-control-auth-stdin), never an env
	// var or argv (both world-readable via /proc on Linux). The daemon launches
	// the agent CLI with its own stdin, so the spawned agent never inherits this
	// fd and cannot read the token. Held in daemon memory only. (MLT-37)
	token, err := randomToken()
	if err != nil {
		return fmt.Errorf("mint control token: %w", err)
	}
	tokenR, tokenW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("control token pipe: %w", err)
	}

	cmd := exec.Command(s.BinPath, args...)
	cmd.Stdin = tokenR     // daemon reads the token as the first line of stdin
	cmd.Stdout = os.Stderr // surface daemon logs in the app's stderr during dev
	cmd.Stderr = os.Stderr
	cmd.Env = envWithOctoBin() // put ~/.xclaw/bin on PATH so the agent can call octo-cli
	if err := cmd.Start(); err != nil {
		tokenR.Close()
		tokenW.Close()
		return fmt.Errorf("start xclawd: %w", err)
	}
	// The child holds its own dup of the read end; write the token and close both
	// ends so the daemon sees a clean newline-terminated line then EOF. Never log it.
	_ = tokenR.Close()
	_, _ = tokenW.WriteString(token + "\n")
	_ = tokenW.Close()

	s.cmd = cmd
	s.authToken = token

	// Wait (briefly) for the daemon to bind the socket before clients dial.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(s.SocketPath); err == nil {
			return nil
		}
		time.Sleep(40 * time.Millisecond)
	}
	return fmt.Errorf("xclawd did not create control socket within timeout")
}

// Stop terminates the daemon. -exit-with-parent also covers the case where we
// die first; here we ask politely, then hard-kill.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

func (s *Supervisor) stopLocked() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _, _ = s.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = s.cmd.Process.Kill()
	}
	s.cmd = nil
	_ = os.Remove(s.SocketPath)
}

// Restart stops the daemon and starts a fresh one (used to apply config changes).
// Held under mu for the whole stop+start so two callers can't interleave.
func (s *Supervisor) Restart() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	return s.startLocked()
}

func binName() string {
	if runtime.GOOS == "windows" {
		return "xclawd.exe"
	}
	return "xclawd"
}

func isExec(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return fi.Mode()&0o111 != 0
}

// randomToken returns a 256-bit cryptographically-random capability token as a
// hex string. Used to mint the control-bus token handed to the daemon at spawn.
func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// daemonArgs builds the xclawd command line. The capability token is NEVER an
// argument (it would be world-readable via /proc/<pid>/cmdline) — it is written
// to the daemon's stdin instead, which -control-auth-stdin tells it to read.
func daemonArgs(socketPath, configPath string) []string {
	args := []string{"-control", socketPath, "-no-repl", "-exit-with-parent", "-control-auth-stdin"}
	if configPath != "" {
		args = append([]string{"-config", configPath}, args...)
	}
	return args
}
