package octo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf16"
)

// --- splitMessageProtected: entity offset rebase across segments -------------

func TestSplitProtectedRebase(t *testing.T) {
	// Build text where a structured mention sits in the SECOND segment.
	// Segment boundary forced by a small maxUnits.
	// "aaaa bbbb @Carl" — resolve @[u1:Carl] near the end.
	input := "aaaaaaaa bbbbbbbb @[u1:Carl] tail"
	res := resolveMentions(input, nil, nil)
	// finalContent = "aaaaaaaa bbbbbbbb @Carl tail"
	wantContent := "aaaaaaaa bbbbbbbb @Carl tail"
	if res.finalContent != wantContent {
		t.Fatalf("content = %q, want %q", res.finalContent, wantContent)
	}
	if len(res.mentionEntries) != 1 {
		t.Fatalf("expected 1 entity, got %+v", res.mentionEntries)
	}
	globalOff := res.mentionEntries[0].Offset // "@Carl" global offset

	ranges := []protectedRange{{start: globalOff, end: globalOff + res.mentionEntries[0].Length}}
	// maxUnits=20 forces a split before "@Carl" (which is at offset 18).
	segs := splitMessageProtected(res.finalContent, 20, ranges)
	if len(segs) < 2 {
		t.Fatalf("expected multiple segments, got %d: %+v", len(segs), segs)
	}

	reassembled, found := assertRebasedMention(t, segs, res.mentionEntries, "@Carl")
	if !found {
		t.Error("mention entity not found wholly within any segment")
	}
	if reassembled != res.finalContent {
		t.Errorf("reassembled = %q, want %q", reassembled, res.finalContent)
	}
}

func assertRebasedMention(t *testing.T, segs []segment, entries []MentionEntity, want string) (string, bool) {
	t.Helper()
	var reassembled string
	found := false
	for _, seg := range segs {
		segStart := seg.start
		segEnd := segStart + utf16Len(seg.text)
		reassembled += seg.text
		for _, e := range entries {
			if e.Offset >= segStart && e.Offset+e.Length <= segEnd {
				local := e.Offset - segStart
				localUnits := utf16.Encode([]rune(seg.text))
				got := decodeUTF16(localUnits[local : local+e.Length])
				if got != want {
					t.Errorf("rebased slice = %q, want %q", got, want)
				}
				found = true
			}
		}
	}
	return reassembled, found
}

func TestSplitProtectedKeepsSpacedNameWhole(t *testing.T) {
	members := map[string]string{"John Smith": "u3"}
	// Put "@John Smith" straddling a natural split point.
	input := "padding padding @John Smith more text here padding"
	res := resolveMentions(input, members, nil)
	ranges := make([]protectedRange, 0, len(res.mentionEntries))
	for _, e := range res.mentionEntries {
		ranges = append(ranges, protectedRange{start: e.Offset, end: e.Offset + e.Length})
	}
	// Force a split right around the mention.
	segs := splitMessageProtected(res.finalContent, 22, ranges)
	// "@John Smith" must appear intact in exactly one segment.
	count := 0
	for _, seg := range segs {
		if strings.Contains(seg.text, "@John Smith") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("@John Smith should be whole in exactly 1 segment, found in %d: %+v", count, segs)
	}
}

// TestSplitProtectedOversizedAtStart: a protected range that begins at offset 0
// and is longer than maxUnits has no earlier boundary to cut at. The splitter
// must keep it whole in one (over-long) segment instead of slicing through it or
// panicking (L22 regression).
func TestSplitProtectedOversizedAtStart(t *testing.T) {
	text := "AAAAAAAAAAAAAAAAAAAA tail" // 20 'A's, then " tail"
	ranges := []protectedRange{{start: 0, end: 20}}
	segs := splitMessageProtected(text, 8, ranges) // maxUnits < protected length
	if len(segs) == 0 {
		t.Fatal("expected at least one segment")
	}
	// The protected run must survive intact at the start of the first segment.
	if !strings.HasPrefix(segs[0].text, "AAAAAAAAAAAAAAAAAAAA") {
		t.Errorf("protected run was sliced: first segment = %q", segs[0].text)
	}
	// Reassembly must be lossless.
	var reassembled string
	for _, s := range segs {
		reassembled += s.text
	}
	if reassembled != text {
		t.Errorf("reassembled = %q, want %q", reassembled, text)
	}
}

// --- OnReply: empty reply → no-response fallback -----------------------------

func TestOnReplyEmptyFallback(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		bodies = append(bodies, b)
		_ = json.NewEncoder(w).Encode(SendMessageResult{MessageID: "1", MessageSeq: 1})
	}))
	defer srv.Close()

	c := NewConnector(NewRESTClient(srv.URL, func() string { return "tok" }))
	c.setCtx(context.Background())
	c.targets["k"] = replyTarget{channelID: "ch", channelType: ChannelDM}

	c.OnReply("k", "   ") // whitespace-only → empty

	if len(bodies) != 1 {
		t.Fatalf("expected 1 send, got %d", len(bodies))
	}
	payload := bodies[0]["payload"].(map[string]any)
	if payload["content"] != noResponseFallback {
		t.Errorf("content = %v, want fallback", payload["content"])
	}
	if _, hasMention := payload["mention"]; hasMention {
		t.Error("fallback message must carry no mention object")
	}

	c.targets["normal"] = replyTarget{channelID: "ch", channelType: ChannelDM}
	c.OnReply("normal", "hello @[u_carl:Carl] @all")
}

func TestOnReplyNoTargetSilent(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	c := NewConnector(NewRESTClient(srv.URL, func() string { return "tok" }))
	c.setCtx(context.Background())
	c.OnReply("missing", "hello") // no target registered
	if called {
		t.Error("expected no REST call when target is unknown")
	}
}
