// Package sandbox provides per-session filesystem isolation for agent turns,
// ported from cc-channel-octo's cwd-resolver.ts + skill-linker.ts.
//
// Each session maps to a deterministic 16-hex sha256 prefix directory under a
// per-bot cwdBase, so one user's working tree cannot be read or mutated from
// another user's session. The partition key is the SAME sessionKey the router
// derives for history (router.InboundMessage.SessionKey), prefixed by the
// channel kind — so the cwd partition can never drift from the history
// partition:
//
//	DM:    sessionKey = "<spaceId>:<uid>" (or bare uid)  → dm:<key>
//	Group: sessionKey = "<channelID>"                    → group:<key>
//
// Group sessionKey is the channel id alone, so all members of a group share one
// sandbox (a group is a collective workspace); DM is per-user (private). The
// kind prefix keeps a DM key and a group key that happen to be byte-identical
// from colliding.
//
// The cwd is a STARTING directory, not a chroot: an agent with Bash can still
// reach absolute paths outside it. Space isolation is provided by one bot per
// space, each with its own cwdBase.
package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Kind classifies the channel a session belongs to.
type Kind string

const (
	KindDM    Kind = "dm"
	KindGroup Kind = "group"
)

// SessionCtx is the routing context used to partition a session's sandbox. Pass
// the router's sessionKey verbatim so cwd/memory partitions track history.
type SessionCtx struct {
	Kind       Kind
	SessionKey string
}

// hashHexLen is the directory-name length: 16 hex = 64 bits, ample for IM use.
const hashHexLen = 16

// registryDirName holds 0-byte provenance markers at the cwdBase root (outside
// the agent's sandbox) so cleanup only ever deletes dirs we provably created.
const registryDirName = ".xclaw-sessions"

// DefaultCwdTTL mirrors store.DefaultTTL: a session sandbox with no bot turn for
// this long is reclaimed.
const DefaultCwdTTL = 7 * 24 * time.Hour

// sessionDirRE guards cleanup: only names matching this exact shape are eligible
// for TTL deletion, so a misconfigured cwdBase can never wipe unrelated files.
var sessionDirRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:hashHexLen]
}

func (c SessionCtx) partitionKey() string {
	// Kind prefix prevents a DM key and a group key that are byte-identical from
	// resolving to the same sandbox.
	return string(c.Kind) + ":" + c.SessionKey
}

// ResolveSessionCwd ensures the per-session cwd exists under cwdBase and returns
// its absolute path. Idempotent — safe to call on every turn. Bumps the dir
// mtime so the TTL tracks last bot turn (not arbitrary filesystem activity).
func ResolveSessionCwd(cwdBase string, ctx SessionCtx) (string, error) {
	name := hashKey(ctx.partitionKey())
	dir := filepath.Join(cwdBase, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("sandbox: mkdir %s: %w", dir, err)
	}

	// Record provenance in the sidecar registry (outside the agent's cwd, so a
	// user-driven agent can neither delete its own marker to evade cleanup nor
	// forge a sibling's). Best effort, re-attempted every resolve so a transient
	// failure self-heals rather than exempting the dir from cleanup forever.
	registryDir := filepath.Join(cwdBase, registryDirName)
	marker := filepath.Join(registryDir, name)
	if _, err := os.Stat(marker); err != nil {
		if mkErr := os.MkdirAll(registryDir, 0o755); mkErr == nil {
			payload, _ := json.Marshal(map[string]string{
				"created": time.Now().UTC().Format(time.RFC3339),
				"kind":    string(ctx.Kind),
			})
			if wErr := os.WriteFile(marker, payload, 0o644); wErr != nil {
				fmt.Fprintf(os.Stderr, "[sandbox] marker write failed for %s: %v\n", dir, wErr)
			}
		}
	}

	// MkdirAll does not touch mtime on an existing dir, so an actively-used
	// session created >TTL ago would be swept on its next turn. Refresh mtime to
	// "now" so the TTL tracks last activity. A concurrent cleanup race must not
	// crash the turn — worst case the dir is recreated next turn.
	now := time.Now()
	if err := os.Chtimes(dir, now, now); err != nil {
		fmt.Fprintf(os.Stderr, "[sandbox] mtime refresh failed for %s: %v\n", dir, err)
	}

	return dir, nil
}

// ResolveMemoryDir computes the per-session auto-memory directory under
// memoryBase. PURE — does NOT mkdir or write a marker (the agent CLI creates it
// on first use). memoryBase lives OUTSIDE cwdBase so the cwd TTL sweep never
// reclaims it: auto-memory is permanent. Uses the same partition key as the cwd
// sandbox so memory tracks the session exactly (group=shared, DM=private).
func ResolveMemoryDir(memoryBase string, ctx SessionCtx) string {
	return filepath.Join(memoryBase, hashKey(ctx.partitionKey()))
}

// CleanupExpiredCwds removes session sandboxes under cwdBase whose mtime is
// older than ttl. Best-effort: failures are logged, never returned — the bot
// must keep running even if disk cleanup hits a permission error. No-op when
// cwdBase does not exist (first run).
//
// A dir is deleted only when ALL hold: name matches the 16-hex pattern, it has a
// matching registry marker (so we never touch another tool's hex-keyed store),
// it is a real directory (lstat, symlinks never followed), and its mtime is past
// the cutoff. The marker is removed alongside the dir.
func CleanupExpiredCwds(cwdBase string, ttl time.Duration) {
	entries, err := os.ReadDir(cwdBase)
	if err != nil {
		return // missing / unreadable — nothing to clean
	}
	registryDir := filepath.Join(cwdBase, registryDirName)
	cutoff := time.Now().Add(-ttl)

	for _, e := range entries {
		name := e.Name()
		if !sessionDirRE.MatchString(name) {
			continue // never touch unrelated files
		}
		marker := filepath.Join(registryDir, name)
		if _, err := os.Stat(marker); err != nil {
			continue // no marker → not ours, leave it untouched at any age
		}
		full := filepath.Join(cwdBase, name)
		// Lstat (not Stat) so a symlinked entry is never followed; a real session
		// dir is always a plain directory.
		info, err := os.Lstat(full)
		if err != nil || !info.IsDir() || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(full); err != nil {
			fmt.Fprintf(os.Stderr, "[sandbox] cleanup failed for %s: %v\n", full, err)
			continue
		}
		_ = os.Remove(marker) // drop the registry entry too
	}
}
