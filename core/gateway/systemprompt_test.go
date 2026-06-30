package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/lml2468/octobuddy/core/config"
	"github.com/lml2468/octobuddy/core/groupctx"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/safety"
	"github.com/lml2468/octobuddy/core/trigger"
)

// dmTurn drives one DM turn through gw and returns the SystemPrompt the driver
// saw on the most recent request.
func dmTurn(t *testing.T, gw *Gateway, drv *fakeDriver, text string) string {
	t.Helper()
	if _, err := gw.Handle(context.Background(), router.InboundMessage{
		ChannelType: router.ChannelDM, FromUID: "u1", FromName: "alice", Text: text,
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(drv.requests) == 0 {
		t.Fatal("driver saw no requests")
	}
	return drv.requests[len(drv.requests)-1].System.Flatten()
}

// TestSystemPromptResolverPerTurn proves the resolver is consulted EVERY turn:
// editing what it returns between turns changes the next turn's system prompt
// with no gateway rebuild (the no-restart guarantee). SecurityPrefix stays first.
func TestSystemPromptResolverPerTurn(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "ok"}
	current := "SOUL-V1"
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), newCaptureSink()).
		WithSystemPrompt("SNAPSHOT").
		WithSystemPromptResolver(func() string { return current })

	sp1 := dmTurn(t, gw, drv, "hi")
	if !strings.HasPrefix(sp1, safety.SecurityPrefix) {
		t.Fatal("SecurityPrefix must be first")
	}
	if !strings.Contains(sp1, "SOUL-V1") || strings.Contains(sp1, "SNAPSHOT") {
		t.Fatalf("turn 1 should use resolver value, not the snapshot: %q", sp1)
	}

	current = "SOUL-V2" // simulate a desktop edit between turns
	sp2 := dmTurn(t, gw, drv, "again")
	if !strings.Contains(sp2, "SOUL-V2") || strings.Contains(sp2, "SOUL-V1") {
		t.Fatalf("turn 2 should reflect the edit without a restart: %q", sp2)
	}
}

// TestSystemPromptResolverNilFallsBackToSnapshot pins that with no resolver the
// static WithSystemPrompt snapshot is used.
func TestSystemPromptResolverNilFallsBackToSnapshot(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "ok"}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), newCaptureSink()).
		WithSystemPrompt("SNAPSHOT-ONLY")

	sp := dmTurn(t, gw, drv, "hi")
	if !strings.Contains(sp, "SNAPSHOT-ONLY") {
		t.Fatalf("nil resolver must fall back to the snapshot: %q", sp)
	}
}

// TestSystemPromptResolverEmptyHonored pins that an empty resolver result is
// honored (operator cleared SOUL.md+AGENTS.md) rather than falling back to the
// snapshot — the prompt drops to SecurityPrefix only.
func TestSystemPromptResolverEmptyHonored(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "ok"}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), newCaptureSink()).
		WithSystemPrompt("SNAPSHOT").
		WithSystemPromptResolver(func() string { return "" })

	sp := dmTurn(t, gw, drv, "hi")
	if strings.Contains(sp, "SNAPSHOT") {
		t.Fatalf("empty resolver must be honored, not replaced by snapshot: %q", sp)
	}
	if strings.TrimSpace(sp) != safety.SecurityPrefix {
		t.Fatalf("cleared prompt should be SecurityPrefix only: %q", sp)
	}
}

const bootstrapMarker = "BOOTSTRAP.md (first-run ritual"

// TestBootstrapHeaderMatchesName is the cross-check that the gateway's local
// bootstrapPromptHeader literal stays in sync with config.BootstrapName (the
// gateway deliberately doesn't import config in production, so this guards
// against the filename drifting between the two packages).
func TestBootstrapHeaderMatchesName(t *testing.T) {
	if !strings.Contains(bootstrapPromptHeader, config.BootstrapName) {
		t.Fatalf("bootstrapPromptHeader %q must contain config.BootstrapName %q", bootstrapPromptHeader, config.BootstrapName)
	}
}

// TestBootstrapInjectionGate pins the owner-gating matrix for the first-run
// ritual: injected for a Console turn and the owner's DM; NEVER for a non-owner
// DM, a group turn, or when the file is absent. SecurityPrefix stays first.
func TestBootstrapInjectionGate(t *testing.T) {
	newGW := func(bootstrap string) (*Gateway, *fakeDriver) {
		drv := &fakeDriver{threadID: "t", reply: "ok"}
		gw := New(drv, newTestStore(t), router.New(router.Config{MaxPerMinute: 100}), newCaptureSink()).
			WithGroupContext(groupctx.New(6000)).
			WithSystemPromptResolver(func() string { return "SOUL" }).
			WithOwner(func() string { return "owner-uid" }).
			WithBootstrapResolver(func() string { return bootstrap })
		return gw, drv
	}
	lastSP := func(drv *fakeDriver) string {
		if len(drv.requests) == 0 {
			t.Fatal("driver saw no requests")
		}
		return drv.requests[len(drv.requests)-1].System.Flatten()
	}

	// Console turn (FromUID is the synthetic gui-user, NOT the owner) → injected.
	gw, drv := newGW("RITUAL-TEXT")
	if _, err := gw.Handle(context.Background(), router.InboundMessage{
		ChannelType: router.ChannelDM, FromUID: "gui-user", FromName: "gui-user",
		Text: "hi", Source: trigger.SourceConsole,
	}); err != nil {
		t.Fatal(err)
	}
	sp := lastSP(drv)
	if !strings.HasPrefix(sp, safety.SecurityPrefix) {
		t.Fatal("SecurityPrefix must stay first")
	}
	if !strings.Contains(sp, bootstrapMarker) || !strings.Contains(sp, "RITUAL-TEXT") {
		t.Fatalf("Console turn must get the bootstrap block: %q", sp)
	}

	// Owner's IM DM → injected.
	gw, drv = newGW("RITUAL-TEXT")
	if _, err := gw.Handle(context.Background(), router.InboundMessage{
		ChannelType: router.ChannelDM, FromUID: "owner-uid", FromName: "owner", Text: "hi",
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lastSP(drv), bootstrapMarker) {
		t.Fatal("owner DM must get the bootstrap block")
	}

	// Non-owner DM → NOT injected.
	gw, drv = newGW("RITUAL-TEXT")
	if _, err := gw.Handle(context.Background(), router.InboundMessage{
		ChannelType: router.ChannelDM, FromUID: "stranger", FromName: "stranger", Text: "hi",
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(lastSP(drv), bootstrapMarker) {
		t.Fatal("non-owner DM must NOT get the bootstrap block")
	}

	// Group turn (even if FromUID == owner) → NOT injected.
	gw, drv = newGW("RITUAL-TEXT")
	if _, err := gw.Handle(context.Background(), router.InboundMessage{
		ChannelType: router.ChannelGroup, ChannelID: "c1", FromUID: "owner-uid", FromName: "owner",
		Text: "hi", Trigger: &trigger.TriggerDecision{Reason: trigger.ReasonExplicitBot, Source: trigger.SourceUser},
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(lastSP(drv), bootstrapMarker) {
		t.Fatal("group turn must NOT get the bootstrap block")
	}

	// File absent (resolver returns "") → NOT injected, even for the owner.
	gw, drv = newGW("")
	if _, err := gw.Handle(context.Background(), router.InboundMessage{
		ChannelType: router.ChannelDM, FromUID: "owner-uid", FromName: "owner", Text: "hi",
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(lastSP(drv), bootstrapMarker) {
		t.Fatal("absent BOOTSTRAP.md must inject nothing")
	}
}
