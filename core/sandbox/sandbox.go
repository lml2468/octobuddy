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
	"fmt"
	"os"
	"path/filepath"
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
// its absolute path. Idempotent — safe to call on every turn. Sandboxes are
// persistent (no TTL reclamation).
func ResolveSessionCwd(cwdBase string, ctx SessionCtx) (string, error) {
	name := hashKey(ctx.partitionKey())
	dir := filepath.Join(cwdBase, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("sandbox: mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// ResolveMemoryDir computes the per-session auto-memory directory under
// memoryBase. PURE — does NOT mkdir (the agent CLI creates it on first use).
// memoryBase lives OUTSIDE cwdBase. Uses the same partition key as the cwd
// sandbox so memory tracks the session exactly (group=shared, DM=private).
func ResolveMemoryDir(memoryBase string, ctx SessionCtx) string {
	return filepath.Join(memoryBase, hashKey(ctx.partitionKey()))
}
