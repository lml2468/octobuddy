package gateway

import (
	"reflect"
	"testing"
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
