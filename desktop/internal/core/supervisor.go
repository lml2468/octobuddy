// Package core supervises the xclawd daemon subprocess for the desktop app:
// it resolves the binary, picks a control-socket path, spawns the daemon in
// control-bus mode (tied to the app's lifetime via -exit-with-parent), and
// stops it cleanly. Reconnect/backoff policy is layered on in Phase 1.
package core

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

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

// Supervisor owns one xclawd process.
type Supervisor struct {
	BinPath    string
	SocketPath string
	ConfigPath string // when non-empty, run -config mode
	cmd        *exec.Cmd
}

// Start spawns xclawd in control-bus mode and waits for the socket to appear.
func (s *Supervisor) Start() error {
	args := []string{"-control", s.SocketPath, "-no-repl", "-exit-with-parent"}
	if s.ConfigPath != "" {
		args = append([]string{"-config", s.ConfigPath}, args...)
	}
	// A stale socket from a crashed prior run would make the daemon's listen fail.
	_ = os.Remove(s.SocketPath)

	cmd := exec.Command(s.BinPath, args...)
	cmd.Stdout = os.Stderr // surface daemon logs in the app's stderr during dev
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start xclawd: %w", err)
	}
	s.cmd = cmd

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
func (s *Supervisor) Restart() error {
	s.Stop()
	return s.Start()
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
