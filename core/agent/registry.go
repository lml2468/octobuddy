package agent

import (
	"fmt"
	"sort"
	"sync"
)

// Options carries the per-bot wiring the daemon supplies when constructing a
// driver through the registry. Every field is a generic seam the gateway/cmd
// layer already owns today (resolved per-Query so background installs, injected
// tokens, and freshly-written MCP configs apply on the next turn without a
// restart). A driver maps them onto its own process invocation; a field a given
// driver doesn't need is simply ignored.
//
// Options is deliberately driver-agnostic: it names NOTHING claude-specific
// (no ANTHROPIC_*, no CLAUDE_CONFIG_DIR). The env-var contract lives behind the
// driver (see EnvMapper); the cmd join-point feeds those values in via EnvFn.
type Options struct {
	// Bin is the agent executable (or "" for the driver's PATH default).
	Bin string
	// BinFn, when set, resolves the binary per-Query (overrides Bin) so a
	// freshly-landed background install is picked up between turns.
	BinFn func() string
	// EnvFn, when set, builds the extra KEY=VALUE env per-Query directly. Used by
	// the low-level/single-bot path that already speaks the driver's env names.
	// Prefer EnvSpecFn (neutral) for config-mode bots.
	EnvFn func() []string
	// EnvSpecFn, when set, supplies the driver-NEUTRAL env spec per-Query; the
	// driver maps it onto its own env-var names via its EnvMapper. This is the
	// agent-agnostic seam: the cmd join-point speaks EnvSpec, the driver owns the
	// naming (Claude → ANTHROPIC_*/CLAUDE_CONFIG_DIR). Ignored when EnvFn is set.
	EnvSpecFn func() EnvSpec
	// MCPConfigFn, when set, resolves the path to this bot's MCP config per-Query
	// ("" when absent). Drivers without an MCP concept ignore it.
	MCPConfigFn func() string
	// ExtraArgs are appended verbatim to the spawned argv (escape hatch).
	ExtraArgs []string
}

// Factory builds a Driver from Options. Each driver registers one under its
// canonical name (the same string its Name() returns).
type Factory func(Options) Driver

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register adds a driver factory under name. Drivers call this from an init()
// so importing the agent package makes them selectable. Panics on a duplicate
// or empty name — both are programmer errors caught at startup, mirroring
// database/sql.Register.
func Register(name string, f Factory) {
	if name == "" {
		panic("agent.Register: empty driver name")
	}
	if f == nil {
		panic("agent.Register: nil factory for " + name)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic("agent.Register: duplicate driver " + name)
	}
	registry[name] = f
}

// New constructs the driver registered under name with opts. An empty name
// selects the default ("claude") so existing config.json files with no
// agent.driver field keep working. An unknown name is an error listing the
// registered drivers, so a config typo fails loudly at startup rather than
// silently falling back to Claude.
func New(name string, opts Options) (Driver, error) {
	if name == "" {
		name = DefaultDriver
	}
	registryMu.RLock()
	f, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown agent driver %q (registered: %v)", name, Registered())
	}
	return f(opts), nil
}

// DefaultDriver is the driver selected when config names none. Claude is the
// reference implementation and the only one shipped enabled by default.
const DefaultDriver = "claude"

// Registered returns the sorted list of registered driver names. Used by New's
// error message so an unknown-driver config surfaces the valid choices.
func Registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ConfigDirNameFor returns the per-bot config-dir base name for the named driver
// (empty name → default), or "" if the driver is unknown or doesn't isolate its
// config (no ConfigDirNamer). Lets the daemon resolve the mkdir / CLAUDE_CONFIG_DIR
// target before fully wiring the driver, keeping the ".claude" literal owned by
// the driver. Constructs a throwaway driver with empty Options — cheap, the
// factory only sets struct fields.
func ConfigDirNameFor(name string) string {
	d, err := New(name, Options{})
	if err != nil {
		return ""
	}
	if n, ok := d.(ConfigDirNamer); ok {
		return n.ConfigDirName()
	}
	return ""
}
