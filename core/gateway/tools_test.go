package gateway

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/clog"
	"github.com/lml2468/octobuddy/core/router"
)

// TestResolveTools pins the per-turn tool-surface resolution through the
// installed resolver: a present channel entry (incl. empty = muzzle) wins;
// otherwise a non-nil bot default; otherwise unset (caller leaves
// Request.AllowedTools nil → driver's probed default).
func TestResolveTools(t *testing.T) {
	def := []string{"Read"}
	channels := map[string][]string{"c1": {"Bash"}, "muz": {}}
	resolver := func(sessionKey string) ([]string, bool) {
		if t, has := channels[sessionKey]; has {
			return t, true
		}
		if def != nil {
			return def, true
		}
		return nil, false
	}
	g := (&Gateway{}).WithToolResolver(resolver)
	if got, ok := g.resolveTools("c1"); !ok || !reflect.DeepEqual(got, []string{"Bash"}) {
		t.Fatalf("channel override: %v ok=%v", got, ok)
	}
	if got, ok := g.resolveTools("muz"); !ok || len(got) != 0 {
		t.Fatalf("muzzle (explicit empty) must be ok with no tools: %v ok=%v", got, ok)
	}
	if got, ok := g.resolveTools("other"); !ok || !reflect.DeepEqual(got, []string{"Read"}) {
		t.Fatalf("fallthrough to default: %v ok=%v", got, ok)
	}

	if _, ok := (&Gateway{}).resolveTools("x"); ok {
		t.Fatal("no resolver configured must be unset (driver default)")
	}
}

// capDriver is a minimal driver whose ToolScoping capability is configurable,
// for the P3 negotiation tests. It scripts a trivial successful turn.
func runToolPolicyTurn(t *testing.T, scoping bool, resolver func(string) ([]string, bool)) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	clog.Setup(false, false, &b)
	t.Cleanup(func() { clog.Setup(false, false, nil) })

	drv := &scriptedDriver{scoping: scoping, script: func(agent.Request, int) ([]agent.AgentEvent, error) {
		return evReply("ok"), nil
	}}
	g := New(drv, newTestStore(t), router.New(router.Config{MaxPerMinute: 100}), newCaptureSink())
	if resolver != nil {
		g = g.WithToolResolver(resolver)
	}
	msg := router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", FromName: "a", Text: "hi"}
	if _, err := g.Handle(context.Background(), msg); err != nil {
		t.Fatalf("handle: %v", err)
	}
	return &b
}

// TestToolPolicyWarnsWhenDriverLacksScoping: a tool policy is set but the driver
// does not enforce scoping (codex-like) → a WARN is emitted. Behavior is
// unchanged (the tools are still passed to the driver); only the silent drop is
// surfaced.
func TestToolPolicyWarnsWhenDriverLacksScoping(t *testing.T) {
	resolver := func(string) ([]string, bool) { return []string{"Read"}, true }
	buf := runToolPolicyTurn(t, false, resolver)
	if !strings.Contains(buf.String(), "tool policy requested but not enforced") {
		t.Fatalf("expected unenforced-tool-policy warning, got: %q", buf.String())
	}
}

// TestToolPolicyNoWarnWhenScopingSupported: a scoping-capable driver (claude-like)
// must not warn.
func TestToolPolicyNoWarnWhenScopingSupported(t *testing.T) {
	resolver := func(string) ([]string, bool) { return []string{"Read"}, true }
	buf := runToolPolicyTurn(t, true, resolver)
	if strings.Contains(buf.String(), "tool policy requested but not enforced") {
		t.Fatalf("scoping-capable driver must not warn, got: %q", buf.String())
	}
}

// TestNoPolicyNoWarn: no tool policy set (resolver returns ok=false) → no warn.
func TestNoPolicyNoWarn(t *testing.T) {
	resolver := func(string) ([]string, bool) { return nil, false }
	buf := runToolPolicyTurn(t, false, resolver)
	if strings.Contains(buf.String(), "tool policy requested but not enforced") {
		t.Fatalf("no policy must not warn, got: %q", buf.String())
	}
}

// TestToolPolicyWarnOncePerSessionDriver: two turns on the same session produce
// exactly one warning.
func TestToolPolicyWarnOncePerSessionDriver(t *testing.T) {
	var b bytes.Buffer
	clog.Setup(false, false, &b)
	t.Cleanup(func() { clog.Setup(false, false, nil) })

	drv := &scriptedDriver{scoping: false, script: func(agent.Request, int) ([]agent.AgentEvent, error) {
		return evReply("ok"), nil
	}}
	g := New(drv, newTestStore(t), router.New(router.Config{MaxPerMinute: 100}), newCaptureSink()).
		WithToolResolver(func(string) ([]string, bool) { return []string{"Read"}, true })
	msg := router.InboundMessage{ChannelType: router.ChannelDM, FromUID: "u1", FromName: "a", Text: "hi"}
	for i := 0; i < 2; i++ {
		if _, err := g.Handle(context.Background(), msg); err != nil {
			t.Fatalf("handle %d: %v", i, err)
		}
	}
	if n := strings.Count(b.String(), "tool policy requested but not enforced"); n != 1 {
		t.Fatalf("want exactly 1 warning across 2 turns, got %d: %q", n, b.String())
	}
}
