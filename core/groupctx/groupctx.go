// Package groupctx maintains per-channel group conversation context for
// injection into the agent prompt, ported from cc-channel-octo's
// group-context.ts. A group is a shared session: every member sees the same
// rolling window. This MVP keeps the window in memory (the SQLite persistence
// in the source can be layered on later); the rendering format and cursor
// semantics are faithful so prompt-safety escaping interoperates exactly.
package groupctx

import (
	"cmp"
	"maps"
	"slices"
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
	// Answered/new segmentation headers (cc G10 / openclaw segmentHistoryEntries
	// in inbound.ts). When a lastBotReplySeq cutoff is known, the delta is split:
	// messages whose IM seq <= cutoff are background the bot already answered;
	// messages with seq > cutoff are new since its last reply. The current message
	// itself is fenced separately by the gateway via safety.CurrentMessageAnchor.
	answeredHeader = "[Previously answered]\n"
	newHeader      = "[New since your last reply]\n"
	// Name/content separator is the FULLWIDTH COLON U+FF1A, matching the source.
	sep = "："
)

type message struct {
	id       int64
	seq      int64 // IM message_seq, for answered/new segmentation (0 = synthetic/cron)
	fromUID  string
	fromName string // already sanitized at ingest
	content  string // stored RAW; escaped by the gateway over the whole block
}

// GroupContext is a concurrency-safe per-channel context store.
type GroupContext struct {
	maxContextChars int

	mu         sync.Mutex
	windows    map[string][]message         // channelID -> chronological window
	cursors    map[string]int64             // channelID -> last injected id
	nextID     map[string]int64             // channelID -> id counter
	nameToUID  map[string]map[string]string // channelID -> name -> uid
	uidToName  map[string]map[string]string // channelID -> uid -> name
	backfilled map[string]bool              // channelID -> cold-start backfill already attempted
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
		backfilled:      map[string]bool{},
	}
}

// BackfillMessage is one historical message returned by a cold-start fetch
// callback (mirrors octo.HistoricalMessage, but IM-agnostic). The seq is the IM
// message_seq used for answered/new segmentation; FromUID lets the caller filter
// the bot's own messages and infer the initial reply cutoff.
type BackfillMessage struct {
	FromUID  string
	FromName string
	Content  string
	Seq      int64
}

// Push caches a message into the channel window (and learns the member name).
// fromName is double-sanitized with a uid fallback, matching the source. seq is
// the IM message_seq (0 for synthetic/cron messages) carried for answered/new
// segmentation in BuildContextSince.
func (g *GroupContext) Push(channelID, fromUID, fromName, content string, seq int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pushLocked(channelID, fromUID, fromName, content, seq)
}

func (g *GroupContext) pushLocked(channelID, fromUID, fromName, content string, seq int64) {
	safeName := safety.SanitizeDisplayName(fromName, "")
	if safeName == "" {
		safeName = safety.SanitizeDisplayName(fromUID, "")
	}
	if safeName == "" {
		safeName = "unknown"
	}

	g.nextID[channelID]++
	id := g.nextID[channelID]
	win := append(g.windows[channelID], message{id: id, seq: seq, fromUID: fromUID, fromName: safeName, content: content})
	if len(win) > maxWindowSize {
		win = win[len(win)-maxWindowSize:]
	}
	g.windows[channelID] = win
	g.learnMemberLocked(channelID, fromUID, safeName)
}

// Backfill seeds an empty channel window once from a fetch callback (cc G4
// cold-start backfill via getChannelMessages). It runs at most once per
// (process, channel): the FIRST time a channel is seen with an empty local
// window. The callback is IM-agnostic — the Octo connector supplies one backed
// by rest.GetChannelMessages. Messages from botUID are skipped (they are the
// bot's own replies) but their highest seq is returned as the inferred initial
// lastBotReplySeq so the very first turn segments correctly. Returns
// (inferredCutoff, ran): ran is false when backfill was already attempted or the
// window is non-empty, so the caller can skip persisting a cursor.
//
// fetch is invoked WITHOUT the mutex held (it does network I/O); the once-guard
// and window check are re-validated under the lock after it returns.
func (g *GroupContext) Backfill(channelID, botUID string, fetch func() []BackfillMessage) (inferredCutoff int64, ran bool) {
	g.mu.Lock()
	if g.backfilled[channelID] || len(g.windows[channelID]) > 0 {
		g.mu.Unlock()
		return 0, false
	}
	// Claim the once-guard BEFORE releasing the lock so a concurrent turn on the
	// same channel can't double-fetch. (Same-session turns serialize via the
	// router lock anyway, but different code paths may call this; be safe.)
	g.backfilled[channelID] = true
	g.mu.Unlock()

	if fetch == nil {
		return 0, false
	}
	msgs := fetch()

	g.mu.Lock()
	defer g.mu.Unlock()
	// Re-check: a live message may have landed while we were fetching. Don't clob.
	if len(g.windows[channelID]) > 0 {
		return 0, true
	}
	for _, m := range msgs {
		if m.FromUID == botUID {
			// Bot's own reply — don't echo it into the window, but use it to infer
			// the initial answered/new cutoff (openclaw inbound.ts cold-start).
			if m.Seq > inferredCutoff {
				inferredCutoff = m.Seq
			}
			continue
		}
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		g.pushLocked(channelID, m.FromUID, m.FromName, m.Content, m.Seq)
	}
	return inferredCutoff, true
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
// highest id seen (the new cursor). The delta is split into two segments by
// cutoffSeq (the bot's last-replied IM message_seq, cc G10 / openclaw
// segmentHistoryEntries): lines whose IM seq <= cutoffSeq render under
// [Previously answered], lines with seq > cutoffSeq under [New since your last
// reply]. When cutoffSeq <= 0 (cold start, nothing answered yet) everything is
// "new" and only the [Recent group messages] header is used, preserving the
// pre-segmentation format. The block is unescaped; the caller wraps it in
// safety.SanitizePromptBody. Returns ("", sinceID) when there is no delta.
func (g *GroupContext) BuildContextSince(channelID string, sinceID, cutoffSeq int64) (text string, lastID int64) {
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

	// Greedy newest-first selection within the char budget (same as the source).
	// Each kept message remembers whether it was already answered so it can be
	// routed into the right segment after selection.
	type sel struct {
		line     string
		answered bool
	}
	var selected []sel
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
		answered := cutoffSeq > 0 && m.seq > 0 && m.seq <= cutoffSeq
		selected = append(selected, sel{line: line, answered: answered})
		used += cost
	}
	if len(selected) == 0 {
		return "", lastID
	}
	// reverse to chronological
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}

	var answered, fresh []string
	for _, s := range selected {
		if s.answered {
			answered = append(answered, s.line)
		} else {
			fresh = append(fresh, s.line)
		}
	}

	// No answered segment (cold start or all-new): keep the single legacy block so
	// the pre-segmentation rendering is unchanged when there's nothing answered.
	if len(answered) == 0 {
		return header + strings.Join(fresh, "\n") + trailer, lastID
	}

	var b strings.Builder
	b.WriteString(header)
	b.WriteString(answeredHeader)
	b.WriteString(strings.Join(answered, "\n"))
	if len(fresh) > 0 {
		b.WriteString("\n")
		b.WriteString(newHeader)
		b.WriteString(strings.Join(fresh, "\n"))
	}
	b.WriteString(trailer)
	return b.String(), lastID
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

// MemberMap returns a snapshot of the channel's displayName→uid map (the shape
// mention-utils.ts's memberMap expects for @name fallback resolution). Returns
// nil when the channel has no known members. The returned map is a copy, safe
// to read without holding g.mu.
func (g *GroupContext) MemberMap(channelID string) map[string]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	src := g.nameToUID[channelID]
	if len(src) == 0 {
		return nil
	}
	return maps.Clone(src)
}

// IsMember reports whether uid is a known member of the channel. Mirrors
// GroupContext.isMember in cc-channel-octo: the best-effort membership predicate
// resolveMentions uses to downgrade hallucinated structured-mention uids to
// plain text. Only as fresh as the last roster/Push learn.
func (g *GroupContext) IsMember(channelID, uid string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.uidToName[channelID][uid]
	return ok
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
	slices.SortFunc(out, func(a, b Member) int {
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return cmp.Compare(a.UID, b.UID)
	})
	return out
}

func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}
