// Package configstore reads and writes the daemon's ~/.xclaw/config.json and the
// per-bot SOUL.md / AGENTS.md files, presenting a flat editor view model. It
// mirrors the legacy Swift ConfigStore: tokens are NEVER written to config.json
// (they live in the OS credential store via the secrets package) and are
// overlaid onto the view model on Load and stripped on Save.
package configstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lml2468/xclaw/core/config"
	"github.com/lml2468/xclaw/core/safepath"
	"github.com/lml2468/xclaw/desktop/internal/octocli"
	"github.com/lml2468/xclaw/desktop/internal/secrets"
)

// saveMu serializes Save so two concurrent saves (e.g. tray + window) can't
// interleave their non-atomic per-bot side effects and, e.g., race removedIDs
// against the keep set. config.json itself is written atomically (temp+rename);
// this guards the surrounding multi-file writes.
var saveMu sync.Mutex

// BotConfig is the flat view model the editor binds to. Tokens are populated
// from the credential store on Load and routed back to it on Save.
type BotConfig struct {
	ID             string            `json:"id"`
	APIURL         string            `json:"apiUrl"`
	Model          string            `json:"model"`
	GatewayBaseURL string            `json:"gatewayBaseUrl"`
	OctoToken      string            `json:"octoToken"`
	GatewayToken   string            `json:"gatewayToken"`
	Env            map[string]string `json:"env"`
	Soul           string            `json:"soul"`
	Agents         string            `json:"agents"`
	// Cron mirrors agent.cron — surface-level toggle for the scheduled-task
	// manager. False (the default) means the bot's cron Manager is never
	// constructed, so the GUI's SchedulesPane shows an "启用并重启" banner
	// instead of an actionable task list. Round-tripped through Save so the
	// SchedulesPane toggle survives a config write.
	Cron bool `json:"cron"`
}

// Dir is ~/.xclaw.
func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw")
}

// Path is ~/.xclaw/config.json.
func Path() string { return filepath.Join(Dir(), "config.json") }

func botDir(id string) string { return filepath.Join(Dir(), id) }

// First-time scaffold for a newly-created bot. Applied only when the file
// does not already exist — writeOrScaffoldBotFile's exists-check (SafeLstat)
// treats both a regular file and an agent-planted symlink as "already there"
// and bails, so an operator who deliberately authored SOUL.md / AGENTS.md
// is never overwritten by a re-save. Kept in English to match the file name
// + project convention; operators in any locale are expected to overwrite these.
const defaultSoulTemplate = `# Identity

<Describe who this bot is — its voice, values, and non-negotiable boundaries.>
`

const defaultAgentsTemplate = `# Behavior

<List observable rules. e.g. Always confirm before destructive actions.
Be concise. Cite sources when asserting facts.>
`

// readFile parses config.json into the daemon's File shape (empty File if absent).
// routes through safepath.SafeRead so an agent (Bash + bypass)
// that plants `~/.xclaw/config.json → /attacker.json` cannot redirect the
// operator-trusted bot roster on the next GUI load.
func readFile() (config.File, error) {
	var f config.File
	home, _ := os.UserHomeDir()
	raw, err := safepath.SafeRead(home, ".xclaw/config.json", 4<<20) // 4 MiB cap
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return f, err
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf("parse config.json: %w", err)
	}
	return f, nil
}

// BotIDs returns just the configured bot ids — used at startup to inject secrets.
func BotIDs() ([]string, error) {
	f, err := readFile()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(f.Bots))
	for _, b := range f.Bots {
		if b.ID != "" {
			ids = append(ids, b.ID)
		}
	}
	return ids, nil
}

// Load returns the editor view of every configured bot: config.json fields
// resolved against the top-level defaults, persona/behavior read from disk, and
// tokens overlaid from the credential store.
func Load() ([]BotConfig, error) {
	f, err := readFile()
	if err != nil {
		return nil, err
	}
	out := make([]BotConfig, 0, len(f.Bots))
	for _, b := range f.Bots {
		out = append(out, resolveBot(f, b))
	}
	return out, nil
}

// LoadOne returns just one bot, doing exactly ONE config.json parse + ONE pair
// of keychain reads + ONE pair of SOUL/AGENTS reads — vs Load, which fans
// the keychain + file work over every bot just to satisfy a single-bot caller.
// Returns (BotConfig{}, false, nil) when the id isn't in config.json; non-nil
// err is reserved for genuine I/O / parse failures.
func LoadOne(botID string) (BotConfig, bool, error) {
	f, err := readFile()
	if err != nil {
		return BotConfig{}, false, err
	}
	for _, b := range f.Bots {
		if b.ID == botID {
			return resolveBot(f, b), true, nil
		}
	}
	return BotConfig{}, false, nil
}

// resolveBot builds a single BotConfig from a parsed File entry. Shared by
// Load (fan-out) and LoadOne (single-bot fast path).
func resolveBot(f config.File, b config.BotEntry) BotConfig {
	bc := BotConfig{ID: b.ID, Env: map[string]string{}}

	bc.APIURL = firstNonEmpty(b.APIURL, f.APIURL)
	// Agent fields: prefer per-bot, fall back to top-level defaults.
	var topModel, topGW string
	var topEnv map[string]string
	if f.Agent != nil {
		topModel, topGW, topEnv = f.Agent.Model, f.Agent.GatewayBaseURL, f.Agent.Env
	}
	// Inherit top-level env only when the per-bot agent block is absent
	// (b.Agent == nil) OR present but env is nil (field absent in JSON).
	// An explicit empty-but-non-nil per-bot env (`agent: {env: {}}`) is
	// an opt-out signal that the operator wants NO inherited env.
	envInherit := b.Agent == nil || b.Agent.Env == nil
	if b.Agent != nil {
		bc.Model = firstNonEmpty(b.Agent.Model, topModel)
		bc.GatewayBaseURL = firstNonEmpty(b.Agent.GatewayBaseURL, topGW)
		if b.Agent.Cron != nil {
			bc.Cron = *b.Agent.Cron
		} else if f.Agent != nil && f.Agent.Cron != nil {
			bc.Cron = *f.Agent.Cron
		}
		maps.Copy(bc.Env, b.Agent.Env)
	} else {
		bc.Model, bc.GatewayBaseURL = topModel, topGW
		if f.Agent != nil && f.Agent.Cron != nil {
			bc.Cron = *f.Agent.Cron
		}
	}
	if envInherit {
		maps.Copy(bc.Env, topEnv)
	}

	bc.Soul = readBotFile(b.ID, "SOUL.md")
	bc.Agents = readBotFile(b.ID, "AGENTS.md")
	bc.OctoToken = secrets.Get(b.ID, secrets.OctoToken)
	bc.GatewayToken = secrets.Get(b.ID, secrets.GatewayToken)
	return bc
}

// Save writes the view model back to disk:
// - config.json: each bot is MERGED onto its existing entry, so per-bot
// overrides the editor doesn't model (rateLimit/context/groupConfigDir/
// onBehalfOf and the mentionFreeGroups/knownBotUids/allowedBotUids/
// botBlocklist gating lists, plus agent.cron/toolProgress) survive a Save;
// editor-owned fields are persisted only when they DIFFER from the
// top-level default (so the defaults keep propagating and aren't frozen
// into N per-bot copies), and tokens are stripped (they live in the OS
// keychain, never config.json);
// - per-bot SOUL.md / AGENTS.md and tokens to the credential store;
// - pruning ONLY the bots named in removedIDs — an explicit deletion list
// from the editor — and only when their on-disk data dir actually exists.
// Pruning is NEVER inferred from a set-difference: a stale editor snapshot
// (a second session, a hand-edit, a restart-rewrite) would otherwise look
// like "every other bot was removed" and irreversibly wipe their data.
//
// config.json is written (atomically) BEFORE the per-bot side effects, so a
// mid-way failure can't leave the index stale while bot files / keychain are
// already mutated; the side effects are idempotent, so a failed Save converges
// on retry.
func Save(bots []BotConfig, removedIDs []string) error {
	saveMu.Lock()
	defer saveMu.Unlock()
	for _, b := range bots {
		if !safepath.ValidSlug(b.ID) {
			return fmt.Errorf("invalid bot id %q — letters, digits, . _ - only", b.ID)
		}
		if err := validURL(b.APIURL); err != nil {
			return fmt.Errorf("bot %s: apiUrl: %w", b.ID, err)
		}
		if b.GatewayBaseURL != "" {
			if err := validURL(b.GatewayBaseURL); err != nil {
				return fmt.Errorf("bot %s: gatewayBaseUrl: %w", b.ID, err)
			}
		}
	}
	if dup := firstDuplicate(bots); dup != "" {
		return fmt.Errorf("duplicate bot id %q", dup)
	}
	if dup := firstDuplicateOctoBotID(bots); dup != "" {
		// Two bots sharing an OCTO_BOT_ID would share an octo-cli disk profile;
		// deleting one bot then runs octocli.Logout for the shared robot id and
		// silently breaks the other bot's auth on its next agent spawn.
		return fmt.Errorf("OCTO_BOT_ID %q is used by more than one bot — each bot needs a distinct Octo robot id", dup)
	}

	// Start from the existing File so top-level defaults + unknown keys survive.
	f, err := readFile()
	if err != nil {
		return err
	}
	existing := make(map[string]config.BotEntry, len(f.Bots))
	for _, e := range f.Bots {
		existing[e.ID] = e
	}
	var topModel, topGW string
	var topEnv map[string]string
	if f.Agent != nil {
		topModel, topGW, topEnv = f.Agent.Model, f.Agent.GatewayBaseURL, f.Agent.Env
	}

	entries := make([]config.BotEntry, 0, len(bots))
	for _, b := range bots {
		// Merge onto the existing entry (zero value for a new bot) so per-bot
		// overrides the editor doesn't model are preserved.
		entry := existing[b.ID]
		entry.ID = b.ID
		entry.OctoToken = "" // tokens live in the OS keychain, never config.json
		entry.APIURL = inheritStr(b.APIURL, f.APIURL)

		ag := config.AgentConfig{}
		if entry.Agent != nil {
			ag = *entry.Agent // preserve cron / toolProgress / any other agent fields
		}
		ag.GatewayToken = "" // keychain only
		ag.Model = inheritStr(b.Model, topModel)
		ag.GatewayBaseURL = inheritStr(b.GatewayBaseURL, topGW)
		// Cron: only materialize per-bot override when it differs from the
		// top-level default — keeps config.json uncluttered when the operator
		// hasn't customized it, and a per-bot pointer (true OR false) properly
		// overrides via mergeAgent.
		var topCron bool
		if f.Agent != nil && f.Agent.Cron != nil {
			topCron = *f.Agent.Cron
		}
		if b.Cron != topCron {
			cron := b.Cron
			ag.Cron = &cron
		} else {
			ag.Cron = nil
		}
		if envEqual(b.Env, topEnv) {
			ag.Env = nil // inherited from the top-level default; don't materialize it
		} else {
			ag.Env = b.Env
		}
		if agentEmpty(ag) {
			entry.Agent = nil
		} else {
			entry.Agent = &ag
		}
		entries = append(entries, entry)
	}
	f.Bots = entries

	// Write config.json first (atomically via temp+rename) — the authoritative
	// index the daemon reads.
	if err := safepath.SafeMkdirAllAbs(Dir(), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := safepath.SafeWriteAbs(Path(), append(raw, '\n'), 0o600); err != nil {
		return err
	}

	// Per-bot side effects (idempotent). On failure, config.json already
	// reflects the intended set and a retry re-applies these.
	for _, b := range bots {
		// was `os.MkdirAll(botDir(b.ID), 0o755)` which
		// follows symlinks at every intermediate component. An agent that
		// plants `~/.xclaw/<newbotID>` as a symlink to `~/.ssh/` BEFORE
		// the first SaveConfig would silently get future SOUL.md writes
		// landing under.ssh — and worse, the operator-trusted prompt
		// source would thereafter be agent-controlled. SafeMkdirAll
		// walks via dirfd, refusing symlinks at every component.
		home, _ := os.UserHomeDir()
		if err := safepath.SafeMkdirAll(home, ".xclaw/"+b.ID, 0o755); err != nil {
			return err
		}
		// SOUL.md / AGENTS.md handling:
		// - operator supplied non-empty content → overwrite (explicit save)
		// - operator left field blank → scaffold the default
		// template atomically via O_CREATE|O_EXCL; no-op if a file
		// already exists.
		// The prior implementation stat'd botDir to derive `firstTime` and
		// then overwrote SOUL.md with the template when that flag was set
		// AND the operator's field was blank — a TOCTOU window where an
		// agent that created SOUL.md between our Stat and our write would
		// have its content silently overwritten. EXCL closes that window.
		// A blank field on an existing bot is intentionally NOT a delete —
		// silent deletion of operator-trusted prompt content from an empty
		// textbox was a footgun.
		if err := writeOrScaffoldBotFile(b.ID, "SOUL.md", b.Soul, defaultSoulTemplate); err != nil {
			return err
		}
		if err := writeOrScaffoldBotFile(b.ID, "AGENTS.md", b.Agents, defaultAgentsTemplate); err != nil {
			return err
		}
		if err := secrets.Set(b.ID, secrets.OctoToken, b.OctoToken); err != nil {
			return fmt.Errorf("store octoToken for %s: %w", b.ID, err)
		}
		if err := secrets.Set(b.ID, secrets.GatewayToken, b.GatewayToken); err != nil {
			return fmt.Errorf("store gatewayToken for %s: %w", b.ID, err)
		}
		// octo-cli profile: when OCTO_BOT_ID is set in the env (which the wizard
		// always does), octo-cli requires a matching disk profile — it does NOT
		// fall back to env-injected OCTO_BOT_TOKEN. Without this, the very first
		// octo-cli call from the agent fails with "no profile found for bot id".
		// Best-effort: a missing octo-cli binary or login failure logs but doesn't
		// fail the save (the operator can repair from the tray's octo-cli row).
		if robotID := strings.TrimSpace(b.Env["OCTO_BOT_ID"]); robotID != "" && b.OctoToken != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if err := octocli.Login(ctx, robotID, b.OctoToken, b.APIURL); err != nil {
				log.Printf("xclaw: octo-cli profile for %s (robot=%s) not synced: %v", b.ID, robotID, err)
			}
			cancel()
		}
	}

	// Prune ONLY explicitly-removed bots, and only when their data dir really
	// exists. Skip any id still present in the saved set.
	keep := make(map[string]bool, len(bots))
	for _, b := range bots {
		keep[b.ID] = true
	}
	for _, id := range removedIDs {
		if keep[id] || !safepath.ValidSlug(id) {
			continue
		}
		// Gate on the bot dir itself, not on its data/ subdir — data/ is
		// only created at daemon startup, so a bot the operator added via
		// the wizard and immediately deleted (before any daemon restart)
		// would otherwise leave ~/.xclaw/<id>/ (with SOUL/AGENTS/secrets/
		// octo profile) orphaned forever. SafeLstat refuses a symlinked
		// bot dir, so the agent-planting concern that motivated this gate
		// still holds.
		if _, err := safepath.SafeLstat(Dir(), id); err != nil {
			continue // bot dir absent (or refused symlinked path) → never RemoveAll
		}
		// was `os.RemoveAll(botDir(id))` which descends
		// into symlinked subdirectories — an agent that planted
		// `~/.xclaw/<id>/data/x → ~/Documents` would have Documents
		// contents unlinked when the operator deleted the bot. SafeRemoveAll
		// (via removeAllAt) opens each subdir with O_NOFOLLOW|O_DIRECTORY
		// so a symlinked entry is unlinked rather than followed.
		home, _ := os.UserHomeDir()
		_ = safepath.SafeRemoveAll(home, ".xclaw/"+id)
		_ = secrets.Delete(id, secrets.OctoToken)
		_ = secrets.Delete(id, secrets.GatewayToken)
		// Clear the matching octo-cli disk profile too, so a re-add of the same
		// robot id doesn't pick up a stale token. existing[id] is the on-disk
		// entry from before this save, so it still carries the bot's env (where
		// OCTO_BOT_ID lives) even though the new bots[] no longer mentions it.
		if prior, ok := existing[id]; ok && prior.Agent != nil {
			if robotID := strings.TrimSpace(prior.Agent.Env["OCTO_BOT_ID"]); robotID != "" {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				if err := octocli.Logout(ctx, robotID); err != nil {
					log.Printf("xclaw: octo-cli profile for %s (robot=%s) not cleared: %v", id, robotID, err)
				}
				cancel()
			}
		}
	}
	return nil
}

// readBotFile reads SOUL.md / AGENTS.md from a bot's dir. Routed through
// safepath.SafeRead so a symlinked file (e.g. an agent-planted
// `~/.xclaw/<id>/SOUL.md → ~/.aws/credentials`) is refused at open time
// instead of having its target read back to the GUI editor.
func readBotFile(id, name string) string {
	if !safepath.ValidSlug(id) {
		return ""
	}
	raw, err := safepath.SafeRead(botDir(id), name, 1<<20) // 1 MiB cap
	if err != nil {
		return ""
	}
	return string(raw)
}

// writeOrScaffoldBotFile is the safe write path for SOUL.md / AGENTS.md.
// Non-empty content overwrites atomically via SafeWriteAbs (leaf-symlink
// refusing). Empty content is a no-op if the file already exists, else
// scaffolds the default template.
func writeOrScaffoldBotFile(id, name, content, tmpl string) error {
	path := filepath.Join(botDir(id), name)
	if strings.TrimSpace(content) != "" {
		return safepath.SafeWriteAbs(path, []byte(content), 0o600)
	}
	// SafeLstat is rooted at botDir(id) directly rather than
	// (home, TrimPrefix(path, home+sep)) — the trimmed-relpath form
	// silently bypassed the exists-check when HOME was unset.
	if fi, err := safepath.SafeLstat(botDir(id), name); err == nil {
		_ = fi // exists (regular file or symlink) — operator's prior save wins, do not touch
		return nil
	}
	return safepath.SafeWriteAbs(path, []byte(tmpl), 0o600)
}

// validURL delegates to the canonical SSRF policy (config.IsAllowedURL) used
// by core/config.Load itself. The prior HasPrefix-based check accepted
// lookalike hosts like `http://localhost.evil.com` and `http://127.0.0.1.attacker.tld`
// — same hazard closed in desktop/internal/octoapi for the wizard's
// POST. Two places hand-rolling the same policy is the drift we're trying to
// stop; reuse the canonical check.
func validURL(s string) error {
	if !config.IsAllowedURL(s) {
		return fmt.Errorf("use https:// (or http://localhost)")
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// inheritStr returns "" when v equals the top-level default def, so the per-bot
// field stays inherited (the default keeps propagating); otherwise it returns v.
// This is the inverse of Load's firstNonEmpty resolution.
func inheritStr(v, def string) string {
	if v == def {
		return ""
	}
	return v
}

func envEqual(a, b map[string]string) bool {
	return maps.Equal(a, b)
}

func agentEmpty(a config.AgentConfig) bool {
	return a.Model == "" && a.GatewayBaseURL == "" && a.GatewayToken == "" &&
		len(a.Env) == 0 && a.Cron == nil && !a.ToolProgress
}

func firstDuplicate(bots []BotConfig) string {
	seen := map[string]bool{}
	for _, b := range bots {
		if seen[b.ID] {
			return b.ID
		}
		seen[b.ID] = true
	}
	return ""
}

// firstDuplicateOctoBotID returns the first OCTO_BOT_ID that appears in more
// than one bot's env. See Save for why this is rejected.
func firstDuplicateOctoBotID(bots []BotConfig) string {
	seen := map[string]bool{}
	for _, b := range bots {
		rid := strings.TrimSpace(b.Env["OCTO_BOT_ID"])
		if rid == "" {
			continue
		}
		if seen[rid] {
			return rid
		}
		seen[rid] = true
	}
	return ""
}
