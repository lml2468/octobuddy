package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lml2468/xclaw/core/control"
)

// TestPeerCredListenerAllowsSameUID verifies the hardened control-bus listener:
// the socket is chmod 0600 and a same-uid client (the test process) connects and
// drives a command end to end. The cross-uid rejection path can't be exercised
// in a unit test without a second uid (needs root to drop privileges), so it is
// covered by the peer-cred check's fail-closed logic + manual review; here we
// prove the gate does not break the legitimate same-uid path.
func TestPeerCredListenerAllowsSameUID(t *testing.T) {
	// Keep the path short: the AF_UNIX sockaddr_un path limit is ~104 bytes, and
	// t.TempDir() on macOS is well over that. Use /tmp with a unique name.
	sock := filepath.Join("/tmp", "xclaw-listen-test-"+filepath.Base(t.TempDir())+".sock")
	defer os.Remove(sock)

	ln := mustListenUnix(sock)
	defer ln.Close()

	// Socket must be owner-only (defense in depth on Linux; harmless elsewhere).
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket perm = %o, want 0600", perm)
	}

	srv := control.NewServer(func(cmdType string, body json.RawMessage) (any, error) {
		return control.HealthBody{Driver: "test", Uptime: 1}, nil
	})
	go srv.Serve(ln)
	defer srv.Close()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	line, _ := control.Encode(control.Envelope{Kind: control.KindCommand, ID: "h1", Type: "health", Body: json.RawMessage(`{}`)})
	if _, err := conn.Write(line); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	sc := control.NewScanner(conn)
	if !sc.Scan() {
		t.Fatalf("no response from server: %v", sc.Err())
	}
	env, err := control.Decode(sc.Bytes())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Kind != control.KindResponse || env.ID != "h1" {
		t.Fatalf("bad response envelope: %+v", env)
	}
}
