package groupctx

import (
	"strings"
	"testing"
)

func TestBuildContextSinceDeltaOnly(t *testing.T) {
	g := New(6000)
	g.Push("c1", "u1", "alice", "first", 1)
	g.Push("c1", "u2", "bob", "second", 2)

	// from cursor 0, both messages are in the delta; cutoff 0 = nothing answered
	text, lastID := g.BuildContextSince("c1", 0, 0)
	assertInitialContextDelta(t, text, lastID)

	// advance cursor; a new message is the only delta
	g.SetCursor("c1", lastID)
	g.Push("c1", "u1", "alice", "third", 3)
	assertContextDeltaAfterCursor(t, g)
}

func assertInitialContextDelta(t *testing.T, text string, lastID int64) {
	t.Helper()

	if lastID != 2 {
		t.Fatalf("lastID = %d, want 2", lastID)
	}
	if !strings.HasPrefix(text, "[Recent group messages]\n") {
		t.Fatalf("missing header: %q", text)
	}
	// fullwidth colon separator, chronological order
	if !strings.Contains(text, "alice："+"first") || !strings.Contains(text, "bob："+"second") {
		t.Fatalf("rendering wrong: %q", text)
	}
	if strings.Index(text, "alice") > strings.Index(text, "bob") {
		t.Fatalf("not chronological: %q", text)
	}
	// no answered cutoff => no segmentation headers (legacy single block)
	if strings.Contains(text, answeredHeader) || strings.Contains(text, newHeader) {
		t.Fatalf("unexpected segmentation headers with cutoff 0: %q", text)
	}
}

func assertContextDeltaAfterCursor(t *testing.T, g *GroupContext) {
	t.Helper()

	text2, lastID2 := g.BuildContextSince("c1", g.Cursor("c1"), 0)
	if lastID2 != 3 || strings.Contains(text2, "first") || !strings.Contains(text2, "third") {
		t.Fatalf("delta after cursor wrong: text=%q lastID=%d", text2, lastID2)
	}
}

func TestBuildContextSinceEmptyDelta(t *testing.T) {
	g := New(6000)
	g.Push("c1", "u1", "a", "hi", 1)
	g.SetCursor("c1", g.MaxID("c1"))
	text, lastID := g.BuildContextSince("c1", g.Cursor("c1"), 0)
	if text != "" || lastID != g.Cursor("c1") {
		t.Fatalf("empty delta should yield empty text + unchanged cursor: %q %d", text, lastID)
	}
}

// TestSegmentationSplitsAnsweredVsNew verifies the [Previously answered] /
// [New since your last reply] split by IM seq vs cutoff (cc G10).
func TestSegmentationSplitsAnsweredVsNew(t *testing.T) {
	g := New(6000)
	g.Push("c1", "u1", "alice", "old-q", 5)      // seq 5 <= cutoff -> answered
	g.Push("c1", "u2", "bob", "answered-too", 7) // seq 7 == cutoff -> answered
	g.Push("c1", "u3", "carol", "new-one", 9)    // seq 9 > cutoff  -> new

	text, _ := g.BuildContextSince("c1", 0, 7)
	if !strings.Contains(text, answeredHeader) {
		t.Fatalf("missing answered header: %q", text)
	}
	if !strings.Contains(text, newHeader) {
		t.Fatalf("missing new header: %q", text)
	}
	// answered segment precedes the new segment
	ai := strings.Index(text, answeredHeader)
	ni := strings.Index(text, newHeader)
	if ai < 0 || ni < 0 || ai > ni {
		t.Fatalf("answered must precede new: %q", text)
	}
	answeredPart := text[ai:ni]
	newPart := text[ni:]
	if !strings.Contains(answeredPart, "old-q") || !strings.Contains(answeredPart, "answered-too") {
		t.Fatalf("answered seg missing answered lines: %q", answeredPart)
	}
	if strings.Contains(answeredPart, "new-one") {
		t.Fatalf("new line leaked into answered seg: %q", answeredPart)
	}
	if !strings.Contains(newPart, "new-one") {
		t.Fatalf("new seg missing new line: %q", newPart)
	}
}

// TestSegmentationAllAnswered: when every delta message is at/below the cutoff,
// only the answered segment renders (no empty new header).
func TestSegmentationAllAnswered(t *testing.T) {
	g := New(6000)
	g.Push("c1", "u1", "alice", "q1", 3)
	g.Push("c1", "u2", "bob", "q2", 4)
	text, _ := g.BuildContextSince("c1", 0, 10)
	if !strings.Contains(text, answeredHeader) {
		t.Fatalf("missing answered header: %q", text)
	}
	if strings.Contains(text, newHeader) {
		t.Fatalf("unexpected new header when nothing is new: %q", text)
	}
}

func TestBudgetDropsOldestKeepsNewest(t *testing.T) {
	// budget = maxContextChars - 25 (header 24 + trailer 1). Each line "n：aaaaa"
	// is 7 UTF-16 units; with the +1 join, two lines need 15. Set budget=10 so
	// only the newest single line fits, but the cursor still advances to max.
	g := New(35) // 35 - 25 = 10
	g.Push("c1", "u", "n", strings.Repeat("a", 5), 1)
	g.Push("c1", "u", "n", strings.Repeat("b", 5), 2)
	text, lastID := g.BuildContextSince("c1", 0, 0)
	if lastID != 2 {
		t.Fatalf("cursor must advance past full delta even when trimmed: %d", lastID)
	}
	if strings.Contains(text, "aaaaa") {
		t.Fatalf("oldest should be dropped under budget: %q", text)
	}
	if !strings.Contains(text, "bbbbb") {
		t.Fatalf("newest should be kept: %q", text)
	}
}

func TestCursorMonotonic(t *testing.T) {
	g := New(6000)
	g.SetCursor("c1", 5)
	g.SetCursor("c1", 3) // backward, ignored
	if g.Cursor("c1") != 5 {
		t.Fatalf("cursor went backward: %d", g.Cursor("c1"))
	}
}

func TestRewindCursor(t *testing.T) {
	g := New(6000)
	g.SetCursor("c1", 10)
	g.RewindCursor("c1", 3) // unconditional, must move backward
	if got := g.Cursor("c1"); got != 3 {
		t.Fatalf("rewind did not roll back: got %d want 3", got)
	}
	g.RewindCursor("c1", 99) // unconditional forward too
	if got := g.Cursor("c1"); got != 99 {
		t.Fatalf("rewind forward failed: got %d want 99", got)
	}
}

func TestPushDoubleSanitizeFallback(t *testing.T) {
	g := New(6000)
	// empty fromName → falls back to (sanitized) uid; bracket in uid stripped
	g.Push("c1", "u[x]", "", "hi", 1)
	text, _ := g.BuildContextSince("c1", 0, 0)
	if strings.Contains(text, "[x]") {
		t.Fatalf("uid fallback not sanitized: %q", text)
	}
	if !strings.Contains(text, "u x"+"："+"hi") {
		t.Fatalf("expected sanitized uid as name: %q", text)
	}
}

func TestResolveMentions(t *testing.T) {
	g := New(6000)
	g.LearnMember("c1", "uid-alice", "alice")
	g.LearnMember("c1", "uid-bob", "bob")
	got := g.ResolveMentions("c1", "hey @alice and @bob!")
	if len(got) != 2 || got[0] != "uid-alice" || got[1] != "uid-bob" {
		t.Fatalf("mentions: %v", got)
	}
	// trailing punctuation stripped; unknown name ignored; dedup
	got2 := g.ResolveMentions("c1", "@alice, @alice @nobody")
	if len(got2) != 1 || got2[0] != "uid-alice" {
		t.Fatalf("dedup/strip/unknown: %v", got2)
	}
	// CJK name
	g.LearnMember("c1", "uid-cjk", "小明")
	got3 := g.ResolveMentions("c1", "你好 @小明")
	if len(got3) != 1 || got3[0] != "uid-cjk" {
		t.Fatalf("cjk mention: %v", got3)
	}
}

// TestBackfillRunsOnce verifies cold-start backfill seeds an empty window from
// the fetch callback exactly once per channel, skips the bot's own messages, and
// infers the initial answered cutoff from the bot's highest reply seq.
func TestBackfillRunsOnce(t *testing.T) {
	g := New(6000)
	calls := 0
	fetch := func() []BackfillMessage {
		calls++
		return []BackfillMessage{
			{FromUID: "user1", FromName: "alice", Content: "hello", Seq: 3},
			{FromUID: "bot", FromName: "Bot", Content: "hi there", Seq: 4}, // bot reply
			{FromUID: "user2", FromName: "bob", Content: "follow up", Seq: 5},
			{FromUID: "user3", FromName: "carol", Content: "   ", Seq: 6}, // empty -> skipped
		}
	}

	cutoff, ran := g.Backfill("c1", "bot", fetch)
	assertFirstBackfill(t, g, calls, cutoff, ran)
	cutoff2, ran2 := g.Backfill("c1", "bot", fetch)
	assertSecondBackfillSkipped(t, calls, cutoff2, ran2)
}

func assertFirstBackfill(t *testing.T, g *GroupContext, calls int, cutoff int64, ran bool) {
	t.Helper()

	if !ran {
		t.Fatal("first backfill should run")
	}
	if calls != 1 {
		t.Fatalf("fetch called %d times, want 1", calls)
	}
	if cutoff != 4 {
		t.Fatalf("inferred cutoff = %d, want 4 (highest bot reply seq)", cutoff)
	}

	// window seeded with non-bot, non-empty messages, in seq order
	text, _ := g.BuildContextSince("c1", 0, 0)
	if !strings.Contains(text, "hello") || !strings.Contains(text, "follow up") {
		t.Fatalf("backfilled window missing user messages: %q", text)
	}
	if strings.Contains(text, "hi there") {
		t.Fatalf("bot's own reply must not be echoed into the window: %q", text)
	}
	if strings.Contains(text, "carol") {
		t.Fatalf("empty-content message must be skipped: %q", text)
	}
}

func assertSecondBackfillSkipped(t *testing.T, calls int, cutoff2 int64, ran2 bool) {
	t.Helper()

	if ran2 {
		t.Fatal("second backfill must not run again")
	}
	if calls != 1 {
		t.Fatalf("fetch must not be called again: calls=%d", calls)
	}
	if cutoff2 != 0 {
		t.Fatalf("no-op backfill should return cutoff 0: %d", cutoff2)
	}
}

// TestBackfillSkippedWhenWindowNonEmpty: a live message present means no
// backfill (the window is already warm).
func TestBackfillSkippedWhenWindowNonEmpty(t *testing.T) {
	g := New(6000)
	g.Push("c1", "u1", "alice", "live", 1)
	called := false
	cutoff, ran := g.Backfill("c1", "bot", func() []BackfillMessage {
		called = true
		return nil
	})
	if ran || called || cutoff != 0 {
		t.Fatalf("backfill should be skipped for a warm window: ran=%v called=%v cutoff=%d", ran, called, cutoff)
	}
}

// TestPushDedupsBySeq: pushing the same (fromUID, seq) twice — a redelivered
// inbound — must NOT add a second window entry, so the LLM context doesn't show
// a duplicated line. A distinct seq from the same user, and seq==0 (synthetic/
// cron, no real seq) are both still appended.
func TestPushDedupsBySeq(t *testing.T) {
	g := New(6000)
	g.Push("c1", "u1", "alice", "hello", 5)
	g.Push("c1", "u1", "alice", "hello", 5) // redelivery — same uid+seq
	text, _ := g.BuildContextSince("c1", 0, 0)
	if n := strings.Count(text, "hello"); n != 1 {
		t.Fatalf("redelivered (uid,seq) must appear once, got %d:\n%s", n, text)
	}

	// A genuinely new message (different seq) is appended.
	g.Push("c1", "u1", "alice", "world", 6)
	text, _ = g.BuildContextSince("c1", 0, 0)
	if !strings.Contains(text, "world") {
		t.Fatalf("distinct seq must be appended:\n%s", text)
	}

	// seq==0 (synthetic/cron) is never deduped — two zero-seq pushes both land.
	g.Push("c2", "u2", "bob", "tick", 0)
	g.Push("c2", "u2", "bob", "tick", 0)
	text, _ = g.BuildContextSince("c2", 0, 0)
	if n := strings.Count(text, "tick"); n != 2 {
		t.Fatalf("seq==0 must not be deduped, got %d", n)
	}
}
