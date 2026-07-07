// Package toolset probes the managed claude binary's tool surface and caches it
// in config.json so the desktop's tool picker can offer the full selectable set
// without spawning claude from the UI. The daemon does NOT read this cache — it
// probes live per turn (see core/agent.ClaudeDriver). This is GUI-only state.
package toolset

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/config"
	"github.com/lml2468/octobuddy/core/safepath"
	"github.com/lml2468/octobuddy/desktop/internal/claudecli"
	"github.com/lml2468/octobuddy/desktop/internal/configstore"
)

// probeConfigDir is a dedicated, plugin-free CLAUDE_CONFIG_DIR the tool-surface
// probe runs under. Probing with the operator's own ~/.claude (the nil-env
// default) inherits their installed MCP plugins, so plugin tool names
// (mcp__plugin_*__*) leak into the "built-in" surface the picker shows. A clean
// isolated dir yields ONLY the binary's genuine built-in tools. Kept stable (not
// a fresh temp dir per call) so the CLI's first-run setup runs once, not every
// probe. Lives beside the managed binary under ~/.octobuddy/bin/.
func probeConfigDir() string {
	return filepath.Join(claudecli.Dir(), ".toolset-probe")
}

// hasMCP reports whether any tool name is an MCP tool (mcp__server__tool). The
// isolated probe never emits these, so their presence in a cached surface means
// the cache predates the isolation fix and must be re-probed.
func hasMCP(tools []string) bool {
	for _, t := range tools {
		if strings.HasPrefix(t, "mcp__") {
			return true
		}
	}
	return false
}

// nowFn is overridable in tests; production uses time.Now.
var nowFn = time.Now

// refreshMu serializes Refresh so concurrent triggers (the install-state hook
// and a LoadToolset background call) don't both spawn claude and race on the
// config.json write. With it, the second caller observes the first's freshly
// written cache and the version-skip short-circuits — at most one probe per
// version change.
var refreshMu sync.Mutex

// Refresh probes the resolved claude binary and, if its tools differ from (or
// the version moved past) the cached entry, persists a fresh ToolsetCache. It's
// a no-op when the binary isn't installed yet. Safe to call on every
// install-state change: it re-probes only when the version changed or nothing
// is cached, so a redundant notification is cheap. Returns the cache in effect.
func Refresh(ctx context.Context) (*config.ToolsetCache, error) {
	refreshMu.Lock()
	defer refreshMu.Unlock()

	bin := claudecli.ResolvedBinPath()
	ver := claudecli.InstalledVersion()

	cached, err := configstore.LoadToolset()
	if err != nil {
		return nil, err
	}
	// Skip the spawn when the cache is already current. With a desktop-managed
	// binary that's "same recorded version". A PATH-managed binary has no
	// recorded version (ver==""); re-probing on every call would spawn claude
	// (~1s) on each settings open, so treat a populated cache as current for
	// the unversioned case too. A genuine binary swap on PATH is picked up by
	// the install-state hook / next daemon restart, not this background poll.
	//
	// EXCEPTION: a cache written by the pre-isolation probe inherited the
	// operator's ~/.claude and so recorded plugin MCP tool names (mcp__*) as
	// "built-ins". The isolated probe below can never emit those, so any mcp__*
	// in the cached surface marks a stale cache — force a re-probe to heal it,
	// even when the version matches.
	if cached != nil && len(cached.Available) > 0 && !hasMCP(cached.Available) {
		if ver != "" && cached.Version == ver {
			return cached, nil
		}
		if ver == "" && cached.Version == "" {
			return cached, nil
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Probe under an isolated, plugin-free CLAUDE_CONFIG_DIR so the reported
	// surface is the binary's genuine built-ins only — NOT the operator's
	// ~/.claude with its installed MCP plugins (whose mcp__plugin_*__* names
	// would otherwise pollute the "built-in" picker). Best-effort mkdir; if it
	// fails we still pass the dir (the CLI creates it).
	dir := probeConfigDir()
	_ = safepath.SafeMkdirAllAbs(dir, 0o700)
	available, err := agent.ProbeTools(ctx, bin, []string{"CLAUDE_CONFIG_DIR=" + dir})
	if err != nil {
		// Probe failed (binary missing/unusable). Leave any existing cache in
		// place; the daemon still probes live per turn regardless.
		return cached, err
	}

	ts := &config.ToolsetCache{
		Version:      ver,
		ProbedAt:     nowFn().Unix(),
		Available:    available,
		HeadlessSafe: agent.HeadlessSafeTools(available),
	}
	if err := configstore.SaveToolset(ts); err != nil {
		return nil, err
	}
	return ts, nil
}
