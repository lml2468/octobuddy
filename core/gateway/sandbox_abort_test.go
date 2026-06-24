package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/store"
)

// TestSandboxFailureAbortsTurn pins the contract that a sandbox-cwd build
// failure aborts the turn BEFORE driver.Query — the agent must never run in the
// process cwd (which would leak files across sessions). It guards the regression
// where prepareAgentRequest's failTurn return (nil-by-old-contract) let runTurn
// fall through to the driver with an empty request.
func TestSandboxFailureAbortsTurn(t *testing.T) {
	base := t.TempDir()
	// Plant a regular file where the cwd base's parent must be a directory, so
	// the per-session mkdir under cwdBase fails deterministically.
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwdBase := filepath.Join(blocker, "workspace") // parent "blocker" is a file → mkdir fails
	memBase := filepath.Join(base, "memory")

	st := newTestStore(t)
	drv := &fakeDriver{threadID: "thr-1", reply: "should never run"}
	sink := newCaptureSink()
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), sink).
		WithSandbox(cwdBase, memBase)

	d, err := gw.Handle(context.Background(),
		router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", FromName: "alice", Text: "hi"})
	if err != nil {
		t.Fatalf("handle returned error (should swallow concluded turn): %v", err)
	}
	if d != router.Accepted {
		t.Fatalf("want Accepted (turn was routed, then aborted internally), got %s", d)
	}

	// The driver must never have been invoked — no agent in the process cwd.
	if len(drv.requests) != 0 {
		t.Fatalf("driver.Query ran on a failed sandbox build: %+v", drv.requests)
	}
	// The user gets exactly the failTurn apology, not the (never produced) reply.
	if sink.replies["u1"] != errorReply {
		t.Fatalf("want errorReply, got %q", sink.replies["u1"])
	}
	// No assistant turn or resume id is persisted for the doomed turn.
	if got, _ := st.Resume("u1", "fake"); got != "" {
		t.Fatalf("resume must not be saved on abort, got %q", got)
	}
	msgs, _ := st.RecentMessages("u1", 10)
	for _, m := range msgs {
		if m.Role == store.RoleAssistant {
			t.Fatalf("assistant message persisted on aborted turn: %+v", msgs)
		}
	}
}
