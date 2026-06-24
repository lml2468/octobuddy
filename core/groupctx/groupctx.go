// Package groupctx maintains per-channel group conversation context for
// injection into the agent prompt, ported from cc-channel-octo's
// group-context.ts. A group is a shared session: every member sees the same
// rolling window. This MVP keeps the window in memory (the SQLite persistence
// in the source can be layered on later); the rendering format and cursor
// semantics are faithful so prompt-safety escaping interoperates exactly.
package groupctx

import (
	"strings"
	"sync"

	"github.com/lml2468/octobuddy/core/safety"
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

// RewindCursor unconditionally sets the cursor — the only path that may
// move it backward. Used by gateway.runTurn to roll back the cursor when
// the turn aborts AFTER buildGroupPrompt has already advanced past the
// current message (e.g. AppendUser failed). Without this the bumped
// cursor would silently exclude the un-persisted message from every
// subsequent [Recent group messages] delta even though every other
// group member saw it on IM.
func (g *GroupContext) RewindCursor(channelID string, lastID int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cursors[channelID] = lastID
}
