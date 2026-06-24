package groupctx

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/lml2468/octobuddy/core/safety"
)

type selectedContextLine struct {
	line     string
	answered bool
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
	delta := collectContextDelta(win, sinceID)
	if len(delta) == 0 {
		return "", sinceID
	}
	lastID = delta[0].id // highest id (delta is newest-first)

	budget := contextLineBudget(g.maxContextChars)
	if budget <= 0 {
		return "", lastID
	}

	selected := selectContextLines(delta, budget, cutoffSeq)
	if len(selected) == 0 {
		return "", lastID
	}
	return renderContextLines(selected), lastID
}

func collectContextDelta(win []message, sinceID int64) []message {
	// collect messages with id > sinceID, newest-first (cap maxWindowSize)
	var delta []message
	for i := len(win) - 1; i >= 0 && len(delta) < maxWindowSize; i-- {
		if win[i].id > sinceID {
			delta = append(delta, win[i])
		}
	}
	return delta
}

func contextLineBudget(maxContextChars int) int {
	return maxContextChars - utf16Len(header) - utf16Len(trailer)
}

func selectContextLines(delta []message, budget int, cutoffSeq int64) []selectedContextLine {
	// Greedy newest-first selection within the char budget (same as the source).
	// Each kept message remembers whether it was already answered so it can be
	// routed into the right segment after selection.
	var selected []selectedContextLine
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
		selected = append(selected, selectedContextLine{line: line, answered: answered})
		used += cost
	}
	// reverse to chronological
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}
	return selected
}

func renderContextLines(selected []selectedContextLine) string {
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
		return header + strings.Join(fresh, "\n") + trailer
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
	return b.String()
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
