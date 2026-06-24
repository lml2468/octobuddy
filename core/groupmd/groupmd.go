// Package groupmd loads operator-authored, per-conversation instruction files
// (GROUP.md / THREAD.md) and injects their contents into the agent system prompt
// as a trusted [Group instructions] block.
//
// It is a faithful Go port of cc-channel-octo's group-config.ts (the simple,
// safe baseline: `<groupConfigDir>/<channelId>.md`, filename-pinned to a safe
// slug, size-capped, group/world-writable files refused) extended with the
// per-thread variant from openclaw-channel-octo's group-md.ts: a thread channel
// id of the form "<groupNo>____<shortId>" prefers its own
// "<groupNo>____<shortId>.md" file and falls back to the parent group's
// "<groupNo>.md".
//
// SECURITY — read carefully (mirrors group-config.ts module header). The
// [Group instructions] block is injected into the system prompt UNSANITIZED, so
// its contents are trusted. That trust holds ONLY if the file is writable solely
// by the operator (the gateway process user). Placing groupConfigDir outside the
// agent-writable cwdBase is necessary but NOT sufficient — under the shipped
// claude defaults (allowedTools '*', bypassPermissions) the agent can write
// absolute paths anywhere the gateway user can. The real protection is OS-level
// perms; as cheap defense-in-depth, Load refuses a group/world-writable file,
// and the channel/thread id is filename-pinned to a safe slug so a crafted id
// cannot traverse out of the directory.
//
// This package is READ-ONLY from disk: the operator edits the files. It does NOT
// implement openclaw's bot-editable update API (out of scope) — only operator
// files injected into the prompt.
package groupmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/lml2468/octobuddy/core/safepath"
)

// MaxBytes caps an instruction file we will inject, keeping the prompt bounded.
// group-config.ts uses 16 KiB; this port uses the 32 KiB sane cap requested for
// the Go version.
const MaxBytes = 32 * 1024

// truncationNotice is appended when a file exceeds MaxBytes (mirrors
// group-config.ts "[… group config truncated]").
const truncationNotice = "\n[… group instructions truncated]"

// threadSep is the thread channel-id separator from openclaw's group-md.ts:
// a thread channelId is "<groupNo>____<shortId>".
const threadSep = "____"

// safeIDRE allows only ids safe as a single path segment — letters, digits, and
// a few separators (mirrors group-config.ts isSafeId). Combined with the
// "." / ".." reject below, a crafted id cannot escape groupConfigDir.
var safeIDRE = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func isSafeID(id string) bool {
	return safeIDRE.MatchString(id) && id != "." && id != ".."
}

// extractParentGroupNo returns the parent group number for a channel id. For a
// thread id "<groupNo>____<shortId>" it returns "<groupNo>"; for a plain group
// id it returns the id unchanged (mirrors group-md.ts extractParentGroupNo).
func extractParentGroupNo(channelID string) string {
	if i := strings.Index(channelID, threadSep); i >= 0 {
		return channelID[:i]
	}
	return channelID
}

// isThreadID reports whether channelID is a thread (community-topic) id.
func isThreadID(channelID string) bool {
	return strings.Contains(channelID, threadSep)
}

// cacheEntry remembers a file's content keyed by its modification state so a
// repeated lookup avoids re-reading disk while still picking up operator edits.
type cacheEntry struct {
	modTime int64  // file mtime UnixNano; 0 means "absent"
	size    int64  // file size at read time
	content string // trimmed, truncated, ready to inject; "" means no injection
}

// Loader resolves and caches per-conversation instruction files under a single
// operator-controlled directory. Safe for concurrent use.
type Loader struct {
	dir string

	mu    sync.Mutex
	cache map[string]cacheEntry // file path -> last-read state
}

// New constructs a Loader rooted at dir. An empty dir yields a Loader whose Load
// always returns ("", false), so callers can wire it unconditionally.
func New(dir string) *Loader {
	return &Loader{dir: dir, cache: map[string]cacheEntry{}}
}

// Load returns the instruction content to inject for a channel, and whether any
// was found. A group channel id loads "<channelId>.md". A thread channel id
// ("<groupNo>____<shortId>") prefers its own "<groupNo>____<shortId>.md" and
// falls back to the parent group's "<groupNo>.md". Missing/empty/unsafe → "".
//
// Never errors: a misconfigured dir or unreadable file degrades to "no custom
// instructions" rather than failing the turn (mirrors group-config.ts).
func (l *Loader) Load(channelID string) (string, bool) {
	if l == nil || l.dir == "" || channelID == "" {
		return "", false
	}

	// Thread: prefer the thread file, then the parent group's file.
	if isThreadID(channelID) {
		if content, ok := l.loadFile(channelID); ok {
			return content, true
		}
		if parent := extractParentGroupNo(channelID); parent != channelID {
			return l.loadFile(parent)
		}
		return "", false
	}

	return l.loadFile(channelID)
}

// loadFile loads "<dir>/<id>.md" with slug-pinning, mtime caching, a size cap,
// and a group/world-writable refusal. All filesystem I/O routes through
// safepath (per CLAUDE.md's policy on path operations under a managed root)
// so a symlinked intermediate cannot satisfy the stat-based perm/mtime check
// while readCapped's SafeOpen rejects the symlink — that drift would silently
// produce no content AND never invalidate the cache slot, leaving the prior
// content live indefinitely. SafeLstat enforces dirfd-walk through every
// parent component AND refuses a leaf symlink, matching readCapped's view.
func (l *Loader) loadFile(id string) (string, bool) {
	if !isSafeID(id) {
		return "", false
	}
	path := filepath.Join(l.dir, id+".md")
	leaf := id + ".md"

	st, err := safepath.SafeLstat(l.dir, leaf)
	if l.rejectUnreadableFile(path, err) {
		return "", false
	}
	if l.rejectNonRegularFile(path, st) {
		return "", false
	}
	if l.rejectWritableFile(path, st) {
		return "", false
	}

	// Fast path: unchanged since last read (mtime + size both match).
	mod := st.ModTime().UnixNano()
	if cached, ok := l.lookupUnchanged(path, mod, st.Size()); ok {
		return cached.content, cached.content != ""
	}

	content := readCapped(l.dir, leaf)
	l.remember(path, cacheEntry{modTime: mod, size: st.Size(), content: content})
	return content, content != ""
}

func (l *Loader) rejectUnreadableFile(path string, err error) bool {
	if err == nil {
		return false
	}
	if !os.IsNotExist(err) {
		// Surface symlink refusal / EACCES / EIO so the operator can tell "no
		// GROUP.md" from "configured but rejected" — the prior silent empty-cache
		// made misconfigured perms or an agent-planted symlink indistinguishable
		// from a missing file.
		fmt.Fprintf(os.Stderr, "[groupmd] refusing %s: %v\n", path, err)
	}
	// Missing, symlinked-leaf-refused, or symlinked-intermediate-refused —
	// remember absence so a repeated miss is cheap; an unrelated load that later
	// succeeds picks up the new content (SafeLstat runs every Load).
	l.remember(path, cacheEntry{})
	return true
}

func (l *Loader) rejectNonRegularFile(path string, st os.FileInfo) bool {
	if st.Mode().IsRegular() {
		return false
	}
	l.remember(path, cacheEntry{modTime: st.ModTime().UnixNano(), size: st.Size()})
	return true
}

func (l *Loader) rejectWritableFile(path string, st os.FileInfo) bool {
	// Defense-in-depth: refuse a group/world-writable file. Its contents are
	// injected UNSANITIZED into the system prompt, so a file anyone-but-the-
	// operator can write is an untrusted injection sink. This is NOT a substitute
	// for proper OS perms — see the package header.
	//
	// Runs BEFORE the cache hot path: a `chmod 0666` that doesn't touch mtime
	// would otherwise keep the previously-cached content live indefinitely
	// (the perm check ran only on the slow path, so a stale cache hit silently
	// bypassed the guard).
	if st.Mode().Perm()&0o022 == 0 {
		return false
	}
	fmt.Fprintf(os.Stderr,
		"[groupmd] refusing %s: file is group/world-writable (mode %04o). Make it writable only by the gateway user.\n",
		path, st.Mode().Perm())
	l.remember(path, cacheEntry{modTime: st.ModTime().UnixNano(), size: st.Size()})
	return true
}

func (l *Loader) lookupUnchanged(path string, modTime, size int64) (cacheEntry, bool) {
	cached, ok := l.lookup(path)
	if ok && cached.modTime == modTime && cached.size == size {
		return cached, true
	}
	return cacheEntry{}, false
}

// readCapped reads at most MaxBytes+1 bytes (so an oversized file can't allocate
// unbounded memory), trims, and appends a truncation notice when over the cap.
// A read error degrades to "". opens via safepath.SafeOpen
// (dirfd-walk + O_NOFOLLOW) so an agent with Bash that plants
// `<groupConfigDir>/<id>.md → ~/.aws/credentials` cannot redirect the
// operator-trusted `[Group instructions]` source — same class of attack
// as the prior SOUL/AGENTS fix, against the SAME trusted region.
func readCapped(dir, leaf string) string {
	f, err := safepath.SafeOpen(dir, leaf)
	if err != nil {
		return ""
	}
	defer f.Close()

	buf := make([]byte, MaxBytes+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return ""
	}
	truncated := n > MaxBytes
	if truncated {
		n = MaxBytes
	}
	content := string(buf[:n])
	if truncated {
		// The cut may land mid-codepoint; drop a trailing partial UTF-8 sequence so
		// we don't emit a lone invalid byte.
		content = trimPartialRune(content)
		content += truncationNotice
	}
	return strings.TrimSpace(content)
}

// trimPartialRune drops a trailing incomplete UTF-8 multibyte sequence.
func trimPartialRune(s string) string {
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError {
			return s
		}
		// RuneError with size 1 on a non-ASCII trailing byte means an incomplete
		// sequence; trim it and re-check.
		if size != 1 {
			return s
		}
		s = s[:len(s)-1]
	}
	return s
}

func (l *Loader) lookup(path string) (cacheEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.cache[path]
	return e, ok
}

func (l *Loader) remember(path string, e cacheEntry) {
	l.mu.Lock()
	l.cache[path] = e
	l.mu.Unlock()
}
