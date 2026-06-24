package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// assertGroupConfigDirOutsideCwd enforces that groupConfigDir (whose files are
// injected UNSANITIZED into the system prompt) is neither the agent-writable
// cwdBase nor nested under it. Otherwise a user-driven agent (default
// allowedTools '*' + bypassPermissions) could write <groupConfigDir>/<id>.md and
// inject its own future trusted instructions. Empty dir = feature off, no check.
//
// Resolves to real paths when they exist (so a symlink can't dodge the boundary)
// and falls back to a lexical clean for not-yet-created dirs. Mirrors
// cc-channel-octo config.ts assertGroupConfigDirOutsideCwd.
func assertGroupConfigDirOutsideCwd(botID, groupConfigDir, cwdBase string) error {
	if groupConfigDir == "" {
		return nil
	}
	gd := canonicalPath(groupConfigDir)
	cb := canonicalPath(cwdBase)
	if cb != "" && (gd == cb || isPathInside(gd, cb)) {
		return fmt.Errorf("bot %q: unsafe groupConfigDir %q — it is the same as or nested under the agent-writable cwdBase %q; "+
			"its files are injected into the system prompt, so it must be operator-controlled and outside the sandbox",
			botID, groupConfigDir, cwdBase)
	}
	return nil
}

// canonicalPath resolves p to its real path when it exists (defeating symlink
// dodges) and otherwise to an absolute lexical clean.
func canonicalPath(p string) string {
	if p == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}

// isPathInside reports whether child is strictly nested under parent.
func isPathInside(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
