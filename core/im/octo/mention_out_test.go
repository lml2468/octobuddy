package octo

import (
	"reflect"
	"testing"
)

// --- resolveMentions: structured @[uid:name] ---------------------------------

func TestResolveStructuredMentions(t *testing.T) {
	for _, tt := range structuredMentionCases() {
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

type structuredMentionCase struct {
	name         string
	input        string
	memberMap    map[string]string
	isValidUid   func(string) bool
	wantContent  string
	wantEntities []MentionEntity
	wantUids     []string
	wantAll      bool
}

func structuredMentionCases() []structuredMentionCase {
	return append(structuredMentionBasicCases(), structuredMentionValidationCases()...)
}

func structuredMentionBasicCases() []structuredMentionCase {
	return []structuredMentionCase{
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
			name:        "CJK display name UTF-16 offsets",
			input:       "嗨 @[u1:小明] 你好",
			wantContent: "嗨 @小明 你好",
			// "嗨 " = 2 UTF-16 units, "@小明" = 3 units.
			wantEntities: []MentionEntity{
				{UID: "u1", Offset: 2, Length: 3},
			},
			wantUids: []string{"u1"},
		},
	}
}

func structuredMentionValidationCases() []structuredMentionCase {
	return []structuredMentionCase{
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
}

// --- resolveMentions: plain @name fallback -----------------------------------

func TestResolvePlainFallback(t *testing.T) {
	members := map[string]string{
		"Alice":      "u1",
		"Bob":        "u2",
		"John Smith": "u3", // name with space — longest-prefix
	}
	for _, tt := range plainFallbackCases() {
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

type plainFallbackCase struct {
	name         string
	input        string
	wantContent  string
	wantEntities []MentionEntity
}

func plainFallbackCases() []plainFallbackCase {
	return []plainFallbackCase{
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
