package groupctx

import (
	"strings"
	"testing"
)

func TestBuildContextSinceDeltaOnly(t *testing.T) {
	g := New(6000)
	g.Push("c1", "u1", "alice", "first")
	g.Push("c1", "u2", "bob", "second")

	// from cursor 0, both messages are in the delta
	text, lastID := g.BuildContextSince("c1", 0)
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

	// advance cursor; a new message is the only delta
	g.SetCursor("c1", lastID)
	g.Push("c1", "u1", "alice", "third")
	text2, lastID2 := g.BuildContextSince("c1", g.Cursor("c1"))
	if lastID2 != 3 || strings.Contains(text2, "first") || !strings.Contains(text2, "third") {
		t.Fatalf("delta after cursor wrong: text=%q lastID=%d", text2, lastID2)
	}
}

func TestBuildContextSinceEmptyDelta(t *testing.T) {
	g := New(6000)
	g.Push("c1", "u1", "a", "hi")
	g.SetCursor("c1", g.MaxID("c1"))
	text, lastID := g.BuildContextSince("c1", g.Cursor("c1"))
	if text != "" || lastID != g.Cursor("c1") {
		t.Fatalf("empty delta should yield empty text + unchanged cursor: %q %d", text, lastID)
	}
}

func TestBudgetDropsOldestKeepsNewest(t *testing.T) {
	// budget = maxContextChars - 25 (header 24 + trailer 1). Each line "n：aaaaa"
	// is 7 UTF-16 units; with the +1 join, two lines need 15. Set budget=10 so
	// only the newest single line fits, but the cursor still advances to max.
	g := New(35) // 35 - 25 = 10
	g.Push("c1", "u", "n", strings.Repeat("a", 5))
	g.Push("c1", "u", "n", strings.Repeat("b", 5))
	text, lastID := g.BuildContextSince("c1", 0)
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

func TestPushDoubleSanitizeFallback(t *testing.T) {
	g := New(6000)
	// empty fromName → falls back to (sanitized) uid; bracket in uid stripped
	g.Push("c1", "u[x]", "", "hi")
	text, _ := g.BuildContextSince("c1", 0)
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
