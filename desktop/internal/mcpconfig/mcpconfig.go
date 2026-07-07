// Package mcpconfig manages a bot's MCP server config file. The standard
// mcp.json lives at ~/.octobuddy/<id>/.claude/.mcp.json (inside the bot's
// CLAUDE_CONFIG_DIR); the daemon loads it per turn via --mcp-config (see
// core/cmd/octobuddy-daemon). The desktop's 基础信息 pane edits it. All file ops
// route through core/safepath, which owns symlink refusal + containment — this
// file has no Lstat / EvalSymlinks concerns of its own.
package mcpconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lml2468/octobuddy/core/safepath"
	"github.com/lml2468/octobuddy/desktop/internal/agentdir"
)

// maxBytes caps a stored mcp.json (it lists a handful of servers — kilobytes).
const maxBytes = 1 << 20 // 1 MiB

// botDir is ~/.octobuddy/<botID>/<agent-config-dir> — the bot's agent config
// dir (Claude: .claude, the CLAUDE_CONFIG_DIR), where the CLI looks for .mcp.json
// when pointed at it via --mcp-config. The config-dir segment is the configured
// driver's, via agentdir.
func botDir(botID string) (string, error) {
	if !safepath.ValidSlug(botID) {
		return "", fmt.Errorf("invalid bot id %q — letters, digits, . _ - only", botID)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".octobuddy", botID, agentdir.Name(botID)), nil
}

// Load returns the raw .mcp.json text for a bot, or "" when none exists. The
// raw text (not a re-marshaled struct) is returned so the editor round-trips
// the operator's exact formatting/comments-free JSON.
func Load(botID string) (string, error) {
	root, err := botDir(botID)
	if err != nil {
		return "", err
	}
	b, err := safepath.SafeRead(root, ".mcp.json", maxBytes)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(b), nil
}

// Save validates and writes a bot's .mcp.json. An empty/whitespace content
// DELETES the file (the "no MCP servers" state) rather than writing an empty
// file the daemon would treat as malformed. Non-empty content must be valid
// JSON with a top-level object `mcpServers` (the standard shape) so a
// fat-fingered paste can't silently disable MCP at the next turn.
func Save(botID, content string) error {
	root, err := botDir(botID)
	if err != nil {
		return err
	}
	if isBlank(content) {
		if err := safepath.SafeRemoveAll(root, ".mcp.json"); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := validate(content); err != nil {
		return err
	}
	// Ensure the bot's .claude dir exists (SafeWrite refuses an absent parent).
	if err := safepath.SafeMkdirAllAbs(root, 0o755); err != nil {
		return err
	}
	return safepath.SafeWrite(root, ".mcp.json", []byte(content), 0o600)
}

// validate enforces the standard mcp.json shape: a JSON object carrying an
// `mcpServers` object. Returns a human-readable error the UI surfaces inline.
func validate(content string) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &top); err != nil {
		return fmt.Errorf("not valid JSON: %w", err)
	}
	raw, ok := top["mcpServers"]
	if !ok {
		return fmt.Errorf("missing top-level \"mcpServers\" object")
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &servers); err != nil {
		return fmt.Errorf("\"mcpServers\" must be an object of server definitions")
	}
	return nil
}

func isBlank(s string) bool {
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
	}
	return true
}
