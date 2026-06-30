package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/lml2468/octobuddy/core/persona"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/safety"
)

// TestPersonaSystemPromptInjection proves a persona clone's system prompt
// carries, in order: the non-overridable SecurityPrefix first, then the
// operator SOUL/AGENTS prompt, then the synthesized persona group hint, then
// the free-form persona prompt.
func TestPersonaSystemPromptInjection(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "ok"}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), newCaptureSink()).
		WithSystemPrompt("SOUL-BODY").
		WithPersona(persona.Grantor{UID: "u_admin", Name: "Admin"}, "always reply in English")

	if _, err := gw.Handle(context.Background(), router.InboundMessage{
		ChannelType: router.ChannelDM, FromUID: "u1", FromName: "alice", Text: "hi",
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if len(drv.requests) != 1 {
		t.Fatalf("want 1 request, got %d", len(drv.requests))
	}
	sp := drv.requests[0].System.Flatten()

	// SecurityPrefix must be first.
	if !strings.HasPrefix(sp, safety.SecurityPrefix) {
		t.Fatal("SecurityPrefix must be first in the system prompt")
	}
	for _, want := range []string{
		"SOUL-BODY",
		"你是Admin的AI分身（persona clone）",
		"你正在以「Admin」的分身身份运作",
		"always reply in English",
	} {
		if !strings.Contains(sp, want) {
			t.Fatalf("system prompt missing %q\n---\n%s", want, sp)
		}
	}
	// Ordering: security < soul < persona-hint < persona-prompt.
	iSec := strings.Index(sp, safety.SecurityPrefix)
	iSoul := strings.Index(sp, "SOUL-BODY")
	iHint := strings.Index(sp, "你是Admin的AI分身")
	iPrompt := strings.Index(sp, "always reply in English")
	if !(iSec < iSoul && iSoul < iHint && iHint < iPrompt) {
		t.Fatalf("ordering wrong: sec=%d soul=%d hint=%d prompt=%d", iSec, iSoul, iHint, iPrompt)
	}
}

// TestNonCloneHasNoPersonaPrompt proves a regular bot's system prompt carries no
// persona instruction.
func TestNonCloneHasNoPersonaPrompt(t *testing.T) {
	st := newTestStore(t)
	drv := &fakeDriver{threadID: "t", reply: "ok"}
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), newCaptureSink()).
		WithSystemPrompt("SOUL-BODY")

	if _, err := gw.Handle(context.Background(), router.InboundMessage{
		ChannelType: router.ChannelDM, FromUID: "u1", FromName: "alice", Text: "hi",
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	sp := drv.requests[0].System.Flatten()
	if strings.Contains(sp, "persona clone") || strings.Contains(sp, "分身") {
		t.Fatalf("non-clone must not carry persona instruction:\n%s", sp)
	}
}
