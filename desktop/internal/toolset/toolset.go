// Package toolset probes the managed claude binary's tool surface and caches it
// in config.json so the desktop's tool picker can offer the full selectable set
// without spawning claude from the UI. The daemon does NOT read this cache — it
// probes live per turn (see core/agent.ClaudeDriver). This is GUI-only state.
package toolset

import (
	"context"
	"sync"
	"time"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/config"
	"github.com/lml2468/octobuddy/desktop/internal/claudecli"
	"github.com/lml2468/octobuddy/desktop/internal/configstore"
)

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
	if cached != nil && len(cached.Available) > 0 {
		if ver != "" && cached.ClaudeVersion == ver {
			return cached, nil
		}
		if ver == "" && cached.ClaudeVersion == "" {
			return cached, nil
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	available, err := agent.ProbeTools(ctx, bin, nil)
	if err != nil {
		// Probe failed (binary missing/unusable). Leave any existing cache in
		// place; the daemon still probes live per turn regardless.
		return cached, err
	}

	ts := &config.ToolsetCache{
		ClaudeVersion: ver,
		ProbedAt:      nowFn().Unix(),
		Available:     available,
		HeadlessSafe:  agent.HeadlessSafeTools(available),
	}
	if err := configstore.SaveToolset(ts); err != nil {
		return nil, err
	}
	return ts, nil
}
