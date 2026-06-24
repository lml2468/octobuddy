package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lml2468/octobuddy/core/groupmd"
	"github.com/lml2468/octobuddy/core/router"
)

// TestGroupMDInjection verifies the [Group instructions] block from a per-channel
// file reaches SystemAppend for a group turn, after the security prefix and SOUL,
// and is absent for a DM turn (which keys on the peer uid, not a shared channel).
func TestGroupMDInjection(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "c1.md"), []byte("Always answer in haiku."), 0o644); err != nil {
		t.Fatal(err)
	}

	gw, drv := newGroupMDGateway(t, dir)

	assertGroupMDGroupTurn(t, gw, drv)
	assertGroupMDDMTurn(t, dir, gw, drv)
}

func newGroupMDGateway(t *testing.T, dir string) (*Gateway, *fakeDriver) {
	t.Helper()

	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "ok"}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), newCaptureSink()).
		WithSystemPrompt("you are OctoBuddy").
		WithGroupMD(groupmd.New(dir))
	return gw, drv
}

func assertGroupMDGroupTurn(t *testing.T, gw *Gateway, drv *fakeDriver) {
	t.Helper()

	if _, err := gw.Handle(context.Background(), router.InboundMessage{
		ChannelType: router.ChannelGroup, ChannelID: "c1", FromUID: "bob", FromName: "bob",
		Text: "hi", Mentioned: true,
	}); err != nil {
		t.Fatal(err)
	}
	if len(drv.requests) != 1 {
		t.Fatalf("want 1 request, got %d", len(drv.requests))
	}
	sp := drv.requests[0].SystemAppend
	if !strings.Contains(sp, "[Group instructions]\nAlways answer in haiku.") {
		t.Fatalf("group instructions missing from system prompt:\n%s", sp)
	}
	// Ordering: security prefix first, SOUL before instructions.
	secIdx := strings.Index(sp, "UNTRUSTED")
	soulIdx := strings.Index(sp, "you are OctoBuddy")
	instrIdx := strings.Index(sp, "[Group instructions]")
	if !(secIdx >= 0 && secIdx < soulIdx && soulIdx < instrIdx) {
		t.Fatalf("ordering wrong: sec=%d soul=%d instr=%d\n%s", secIdx, soulIdx, instrIdx, sp)
	}
}

func assertGroupMDDMTurn(t *testing.T, dir string, gw *Gateway, drv *fakeDriver) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, "dave.md"), []byte("secret persona"), 0o644); err != nil {
		t.Fatal(err)
	}
	drv.requests = nil
	if _, err := gw.Handle(context.Background(), router.InboundMessage{
		ChannelType: router.ChannelDM, FromUID: "dave", FromName: "dave", Text: "hi",
	}); err != nil {
		t.Fatal(err)
	}
	if len(drv.requests) != 1 {
		t.Fatalf("want 1 DM request, got %d", len(drv.requests))
	}
	if strings.Contains(drv.requests[0].SystemAppend, "[Group instructions]") {
		t.Fatalf("DM turn must not inject group instructions:\n%s", drv.requests[0].SystemAppend)
	}
}

// TestGroupMDDisabled confirms no injection (and no panic) when no loader is set.
func TestGroupMDDisabled(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "ok"}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), newCaptureSink())

	if _, err := gw.Handle(context.Background(), router.InboundMessage{
		ChannelType: router.ChannelGroup, ChannelID: "c1", FromUID: "bob", Text: "hi", Mentioned: true,
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(drv.requests[0].SystemAppend, "[Group instructions]") {
		t.Fatalf("no loader should mean no injection:\n%s", drv.requests[0].SystemAppend)
	}
}
