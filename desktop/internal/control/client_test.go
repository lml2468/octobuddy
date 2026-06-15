package control_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lml2468/xclaw/desktop/internal/control"
	"github.com/lml2468/xclaw/desktop/internal/core"
)

// TestBridgeHealthRoundTrip spawns the real xclawd daemon via the supervisor,
// connects the control client over the Unix socket, and asserts a health
// command round-trips back as a response envelope. This is the Phase 0 proof
// that "Go reads the UDS stream + commands work" end-to-end. It skips when the
// daemon binary isn't available (so it never blocks CI without a build).
func TestBridgeHealthRoundTrip(t *testing.T) {
	bin := os.Getenv("XCLAWD_BIN")
	if bin == "" {
		// Fall back to the monorepo dev build if present.
		if wd, err := os.Getwd(); err == nil {
			// .../desktop/internal/control -> repo root is three up.
			cand := filepath.Join(wd, "..", "..", "..", "core", ".xclawd-dev")
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				bin = cand
			}
		}
	}
	if bin == "" {
		t.Skip("no xclawd binary (set XCLAWD_BIN or build core/.xclawd-dev)")
	}

	// Short path under /tmp — sockaddr_un caps at ~104 bytes, and macOS t.TempDir()
	// paths blow past that.
	sock := filepath.Join("/tmp", "xclaw-test.sock")
	_ = os.Remove(sock)
	defer os.Remove(sock)
	sup := &core.Supervisor{
		BinPath:    bin,
		SocketPath: sock,
		// single-bot mode (no config) — health works without an LLM.
	}
	if err := sup.Start(); err != nil {
		t.Fatalf("supervisor start: %v", err)
	}
	defer sup.Stop()

	client, err := control.Dial(sup.SocketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	got := make(chan control.HealthBody, 1)
	go func() {
		_ = client.Read(func(env control.Envelope) {
			if env.Kind == control.KindResponse && env.Type == "health" {
				var h control.HealthBody
				if json.Unmarshal(env.Body, &h) == nil {
					select {
					case got <- h:
					default:
					}
				}
			}
		})
	}()

	if _, err := client.Send("health", nil); err != nil {
		t.Fatalf("send health: %v", err)
	}

	select {
	case h := <-got:
		if h.Driver == "" {
			t.Fatalf("health response missing driver: %+v", h)
		}
		t.Logf("health ok: driver=%s bots=%d conns=%d uptime=%ds", h.Driver, h.Bots, h.Connections, h.Uptime)
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for health response")
	}
}
