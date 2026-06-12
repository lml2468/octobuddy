package octo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

// --- resolveMentions: structured @[uid:name] ---------------------------------

func TestResolveStructuredMentions(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		memberMap    map[string]string
		isValidUid   func(string) bool
		wantContent  string
		wantEntities []MentionEntity
		wantUids     []string
		wantAll      bool
	}{
		{
			name:        "single structured mention",
			input:       "hey @[u1:Alice] welcome",
			wantContent: "hey @Alice welcome",
			wantEntities: []MentionEntity{
				{UID: "u1", Offset: 4, Length: 6}, // "@Alice"
			},
			wantUids: []string{"u1"},
		},
		{
			name:        "two structured mentions, offsets track replacement length",
			input:       "@[u1:Al] and @[u2:Bob] go",
			wantContent: "@Al and @Bob go",
			wantEntities: []MentionEntity{
				{UID: "u1", Offset: 0, Length: 3}, // "@Al"
				{UID: "u2", Offset: 8, Length: 4}, // "@Bob"
			},
			wantUids: []string{"u1", "u2"},
		},
		{
			name:         "hallucinated uid downgraded to plain text when isValidUid fails",
			input:        "ping @[ghost:Nobody] please",
			isValidUid:   func(uid string) bool { return uid == "real" },
			wantContent:  "ping @Nobody please", // text stays, entity dropped
			wantEntities: nil,
			wantUids:     []string{},
		},
		{
			name:        "valid uid kept when isValidUid passes",
			input:       "ping @[real:R] now",
			isValidUid:  func(uid string) bool { return uid == "real" },
			wantContent: "ping @R now",
			wantEntities: []MentionEntity{
				{UID: "real", Offset: 5, Length: 2},
			},
			wantUids: []string{"real"},
		},
		{
			name:        "CJK display name UTF-16 offsets",
			input:       "嗨 @[u1:小明] 你好",
			wantContent: "嗨 @小明 你好",
			// "嗨 " = 2 UTF-16 units, "@小明" = 3 units.
			wantEntities: []MentionEntity{
				{UID: "u1", Offset: 2, Length: 3},
			},
			wantUids: []string{"u1"},
		},
		{
			name:        "astral (emoji) before mention shifts UTF-16 offset by 2",
			input:       "😀@[u1:A]",
			wantContent: "😀@A",
			// emoji = 2 UTF-16 units, so "@A" starts at offset 2.
			wantEntities: []MentionEntity{
				{UID: "u1", Offset: 2, Length: 2},
			},
			wantUids: []string{"u1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := resolveMentions(tt.input, tt.memberMap, tt.isValidUid)
			if res.finalContent != tt.wantContent {
				t.Errorf("content = %q, want %q", res.finalContent, tt.wantContent)
			}
			if !reflect.DeepEqual(res.mentionEntries, tt.wantEntities) {
				t.Errorf("entities = %+v, want %+v", res.mentionEntries, tt.wantEntities)
			}
			if !reflect.DeepEqual(res.mentionUids, tt.wantUids) {
				t.Errorf("uids = %+v, want %+v", res.mentionUids, tt.wantUids)
			}
			if res.mentionAll != tt.wantAll {
				t.Errorf("mentionAll = %v, want %v", res.mentionAll, tt.wantAll)
			}
		})
	}
}

// --- resolveMentions: plain @name fallback -----------------------------------

func TestResolvePlainFallback(t *testing.T) {
	members := map[string]string{
		"Alice":      "u1",
		"Bob":        "u2",
		"John Smith": "u3", // name with space — longest-prefix
	}
	tests := []struct {
		name         string
		input        string
		wantContent  string
		wantEntities []MentionEntity
	}{
		{
			name:        "simple @name resolves",
			input:       "hi @Alice",
			wantContent: "hi @Alice",
			wantEntities: []MentionEntity{
				{UID: "u1", Offset: 3, Length: 6},
			},
		},
		{
			name:        "longest-prefix match for name with space",
			input:       "yo @John Smith here",
			wantContent: "yo @John Smith here",
			wantEntities: []MentionEntity{
				{UID: "u3", Offset: 3, Length: 11}, // "@John Smith"
			},
		},
		{
			name:         "email is not a mention (lead boundary blacklist)",
			input:        "mail user@Alice.com",
			wantContent:  "mail user@Alice.com",
			wantEntities: nil,
		},
		{
			name:         "unknown name produces no entity",
			input:        "hi @Carol",
			wantContent:  "hi @Carol",
			wantEntities: nil,
		},
		{
			name:        "structured takes precedence; same offset not double-counted",
			input:       "@[u1:Alice] hi",
			wantContent: "@Alice hi",
			wantEntities: []MentionEntity{
				{UID: "u1", Offset: 0, Length: 6},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := resolveMentions(tt.input, members, nil)
			if res.finalContent != tt.wantContent {
				t.Errorf("content = %q, want %q", res.finalContent, tt.wantContent)
			}
			if !reflect.DeepEqual(res.mentionEntries, tt.wantEntities) {
				t.Errorf("entities = %+v, want %+v", res.mentionEntries, tt.wantEntities)
			}
		})
	}
}

// --- resolveMentions: @all / @所有人 detection --------------------------------

func TestResolveMentionAll(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"@all at start", "@all gather", true},
		{"@all after space", "ok @all now", true},
		{"@所有人 CJK", "请 @所有人 注意", true},
		{"@所有人 followed by CJK punct", "@所有人，开会", true},
		{"case-insensitive @ALL", "@ALL listen", true},
		{"@all-foo does NOT broadcast (hyphen is name char)", "@all-foo", false},
		{"@all.x does NOT broadcast (dot is name char)", "@all.x", false},
		{"@allen is a name, not all", "@allen", false},
		{"email-like not all", "x@all.com", false},
		{"@all at end of string", "everyone @all", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectMentionAll(tt.input); got != tt.want {
				t.Errorf("detectMentionAll(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestResolveMentionAllSkipsEntity(t *testing.T) {
	// @all must NOT become an entity even if "all" is a member name collision.
	members := map[string]string{"all": "uAll"}
	res := resolveMentions("@all hi", members, nil)
	if !res.mentionAll {
		t.Error("expected mentionAll=true")
	}
	if len(res.mentionEntries) != 0 {
		t.Errorf("expected no entities for @all, got %+v", res.mentionEntries)
	}
}

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

	// Reassemble and check the mention lands wholly in one segment, with a
	// correctly rebased local offset.
	var reassembled string
	found := false
	for _, seg := range segs {
		segStart := seg.start
		segEnd := segStart + utf16Len(seg.text)
		reassembled += seg.text
		for _, e := range res.mentionEntries {
			if e.Offset >= segStart && e.Offset+e.Length <= segEnd {
				local := e.Offset - segStart
				localUnits := utf16.Encode([]rune(seg.text))
				got := decodeUTF16(localUnits[local : local+e.Length])
				if got != "@Carl" {
					t.Errorf("rebased slice = %q, want %q", got, "@Carl")
				}
				found = true
			}
		}
	}
	if !found {
		t.Error("mention entity not found wholly within any segment")
	}
	if reassembled != res.finalContent {
		t.Errorf("reassembled = %q, want %q", reassembled, res.finalContent)
	}
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
	c.runCtx = context.Background()
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
}

func TestOnReplyNoTargetSilent(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	c := NewConnector(NewRESTClient(srv.URL, func() string { return "tok" }))
	c.runCtx = context.Background()
	c.OnReply("missing", "hello") // no target registered
	if called {
		t.Error("expected no REST call when target is unknown")
	}
}
