// Package agentdir resolves the per-bot agent config-directory NAME (the base
// segment under ~/.octobuddy/<id>/) for whichever driver a bot is configured to
// use. Claude uses ".claude"; a different driver uses its own (Codex: ".codex").
//
// This is the single desktop-side source of truth for that segment, so the
// skills / workflows / mcpconfig packages — which write into the same directory
// the agent CLI reads from — never hardcode ".claude" and never drift from the
// driver's own ConfigDirName() contract in core/agent.
//
// The driver name is read from config.json's per-bot agent.driver; an empty or
// unknown driver, or any read error, falls back to ".claude" (the default
// driver) so the desktop degrades safely rather than failing a CRUD op.
package agentdir

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/safepath"
)

// defaultName mirrors the default driver's config dir; used whenever the bot's
// driver can't be resolved. Kept as a literal (not agent.ConfigDirNameFor(""))
// so a misconfigured registry still yields a sane directory.
const defaultName = ".claude"

// Name returns the config-dir base name for botID's configured driver, e.g.
// ".claude" or ".codex". Falls back to ".claude" on any error.
func Name(botID string) string {
	driver := driverFor(botID)
	if n := agent.ConfigDirNameFor(driver); n != "" {
		return n
	}
	return defaultName
}

// driverFor reads botID's agent.driver from config.json with a minimal,
// validation-free parse (the desktop must resolve the dir even when the full
// config wouldn't pass Load's SSRF/slug checks). "" when absent or unreadable —
// Name then resolves it to the default driver.
func driverFor(botID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, ".octobuddy", "config.json")
	b, err := safepath.SafeReadAbs(path, 1<<20)
	if err != nil {
		return ""
	}
	var f struct {
		Bots []struct {
			ID    string `json:"id"`
			Agent *struct {
				Driver string `json:"driver"`
			} `json:"agent"`
		} `json:"bots"`
	}
	if json.Unmarshal(b, &f) != nil {
		return ""
	}
	for _, bot := range f.Bots {
		if bot.ID == botID && bot.Agent != nil {
			return bot.Agent.Driver
		}
	}
	return ""
}
