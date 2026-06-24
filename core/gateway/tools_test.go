package gateway

import (
	"reflect"
	"testing"
)

// TestResolveTools pins the per-turn tool-surface resolution: a present channel
// entry (incl. empty = muzzle) wins; otherwise a non-nil bot default; otherwise
// unset (caller leaves Request.AllowedTools nil → driver's probed default).
func TestResolveTools(t *testing.T) {
	g := &Gateway{
		toolDefault:  []string{"Read"},
		toolChannels: map[string][]string{"c1": {"Bash"}, "muz": {}},
	}
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
		t.Fatal("no policy configured must be unset (driver default)")
	}
}
