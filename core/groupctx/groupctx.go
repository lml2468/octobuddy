// Package groupctx maintains per-channel group conversation context for
// injection into the agent prompt, ported from cc-channel-octo's
// group-context.ts. A group is a shared session: every member sees the same
// rolling window. This MVP keeps the window in memory (the SQLite persistence
// in the source can be layered on later); the rendering format and cursor
// semantics are faithful so prompt-safety escaping interoperates exactly.
package groupctx

import (
	"sort"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/lml2468/xclaw/core/safety"
)

const (
	maxWindowSize = 100
	// header "[Recent group messages]\n" + trailer "\n".
	header  = "[Recent group messages]\n"
	trailer = "\n"
	// Name/content separator is the FULLWIDTH COLON U+FF1A, matching the source.
	sep = "："
)

type message struct {
	id       int64
	fromUID  string
	fromName string // already sanitized at ingest
	content  string // stored RAW; escaped by the gateway over the whole block
}

// GroupContext is a concurrency-safe per-channel context store.
type GroupContext struct {
	maxContextChars int

	mu        sync.Mutex
	windows   map[string][]message         // channelID -> chronological window
	cursors   map[string]int64             // channelID -> last injected id
	nextID    map[string]int64             // channelID -> id counter
	nameToUID map[string]map[string]string // channelID -> name -> uid
	uidToName map[string]map[string]string // channelID -> uid -> name
}

// New constructs a GroupContext with the given char budget (config
// context.maxContextChars, default 6000).
func New(maxContextChars int) *GroupContext {
	if maxContextChars <= 0 {
		maxContextChars = 6000
	}
	return &GroupContext{
		maxContextChars: maxContextChars,
		windows:         map[string][]message{},
		cursors:         map[string]int64{},
		nextID:          map[string]int64{},
		nameToUID:       map[string]map[string]string{},
		uidToName:       map[string]map[string]string{},
	}
}

// Push caches a message into the channel window (and learns the member name).
// fromName is double-sanitized with a uid fallback, matching the source.
func (g *GroupContext) Push(channelID, fromUID, fromName, content string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	safeName := safety.SanitizeDisplayName(fromName, "")
	if safeName == "" {
		safeName = safety.SanitizeDisplayName(fromUID, "")
	}
	if safeName == "" {
		safeName = "unknown"
	}

	g.nextID[channelID]++
	id := g.nextID[channelID]
	win := append(g.windows[channelID], message{id: id, fromUID: fromUID, fromName: safeName, content: content})
	if len(win) > maxWindowSize {
		win = win[len(win)-maxWindowSize:]
	}
	g.windows[channelID] = win
	g.learnMemberLocked(channelID, fromUID, safeName)
}

// Cursor returns the channel's current injection cursor.
func (g *GroupContext) Cursor(channelID string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cursors[channelID]
}

// MaxID returns the highest message id currently in the channel window.
func (g *GroupContext) MaxID(channelID string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.nextID[channelID]
}

// SetCursor advances the cursor monotonically (never backward).
func (g *GroupContext) SetCursor(channelID string, lastID int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if lastID > g.cursors[channelID] {
		g.cursors[channelID] = lastID
	}
}

// BuildContextSince renders the messages strictly newer than sinceID, capped by
// the char budget (UTF-16 units), and returns the rendered RAW block plus the
// highest id seen (the new cursor). The block is unescaped; the caller wraps it
// in safety.SanitizePromptBody. Returns ("", sinceID) when there is no delta.
func (g *GroupContext) BuildContextSince(channelID string, sinceID int64) (text string, lastID int64) {
	g.mu.Lock()
	defer g.mu.Unlock()

	win := g.windows[channelID]
	// collect messages with id > sinceID, newest-first (cap maxWindowSize)
	var delta []message
	for i := len(win) - 1; i >= 0 && len(delta) < maxWindowSize; i-- {
		if win[i].id > sinceID {
			delta = append(delta, win[i])
		}
	}
	if len(delta) == 0 {
		return "", sinceID
	}
	lastID = delta[0].id // highest id (delta is newest-first)

	budget := g.maxContextChars - utf16Len(header) - utf16Len(trailer)
	if budget <= 0 {
		return "", lastID
	}

	var selected []string
	used := 0
	for _, m := range delta { // newest -> oldest
		line := m.fromName + sep + m.content
		cost := utf16Len(line)
		if len(selected) > 0 {
			cost++ // joining "\n"
		}
		if used+cost > budget {
			break
		}
		selected = append(selected, line)
		used += cost
	}
	if len(selected) == 0 {
		return "", lastID
	}
	// reverse to chronological
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}
	return header + strings.Join(selected, "\n") + trailer, lastID
}

// ResolveMentions maps @name tokens in text to uids for the channel.
func (g *GroupContext) ResolveMentions(channelID, text string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	names := g.nameToUID[channelID]
	if len(names) == 0 {
		return nil
	}
	matches := mentionRE.FindAllStringSubmatch(text, -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		name := strings.TrimRight(m[1], mentionTrailingPunct)
		if name == "" {
			continue
		}
		if uid, ok := names[name]; ok && !seen[uid] {
			seen[uid] = true
			out = append(out, uid)
		}
	}
	return out
}

// LearnMember records a uid↔name mapping (e.g. from a member roster refresh).
func (g *GroupContext) LearnMember(channelID, uid, name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.learnMemberLocked(channelID, uid, safety.SanitizeDisplayName(name, ""))
}

func (g *GroupContext) learnMemberLocked(channelID, uid, name string) {
	if uid == "" || name == "" {
		return
	}
	if g.uidToName[channelID] == nil {
		g.uidToName[channelID] = map[string]string{}
		g.nameToUID[channelID] = map[string]string{}
	}
	if old, ok := g.uidToName[channelID][uid]; ok && old != name {
		// only delete the reverse mapping if it still points to this uid
		if g.nameToUID[channelID][old] == uid {
			delete(g.nameToUID[channelID], old)
		}
	}
	g.uidToName[channelID][uid] = name
	g.nameToUID[channelID][name] = uid
}

// Member is a learned uid↔name pair for a channel.
type Member struct {
	UID  string
	Name string
}

// Members returns the channel's learned roster (uid + sanitized name), sorted
// by name then uid for deterministic output. Push learns members as messages
// arrive; LearnMember can seed them from a roster refresh.
func (g *GroupContext) Members(channelID string) []Member {
	g.mu.Lock()
	defer g.mu.Unlock()
	m := g.uidToName[channelID]
	if len(m) == 0 {
		return nil
	}
	out := make([]Member, 0, len(m))
	for uid, name := range m {
		out = append(out, Member{UID: uid, Name: name})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].UID < out[j].UID
	})
	return out
}

func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}
