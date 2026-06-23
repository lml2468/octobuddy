package octo

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf16"
)

// Outbound @mention resolution, ported from cc-channel-octo's mention-utils.ts
// (and the deliver splitting in stream-relay.ts).
//
// Two formats are recognized in agent output:
// - v2 structured: @[uid:displayName] — precise, generated from the system
// prompt; converted to a human-readable @displayName plus a MentionEntity.
// - v1 plain: @name — resolved against the channel roster (displayName → uid).
//
// @all / @所有人 collapse into a single mentionAll flag.
//
// Offsets/lengths are in UTF-16 code units to match the Octo wire contract
// (octo/types.ts: "offset/length 的单位为 UTF-16 code units"), so they are
// byte-identical to what the TS adapter emits. Go's regexp (RE2) has no
// lookbehind, so the JS lookbehind/lookahead boundary checks are emulated
// manually against a []rune view of the text.

// MentionEntity is the precise position of one resolved @name within content.
// Mirrors octo/types.ts MentionEntity; offset/length are UTF-16 code units and
// include the leading '@'.
type MentionEntity struct {
	UID    string `json:"uid"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

// structuredMention is one parsed @[uid:name], with its position in the SOURCE
// text expressed in UTF-16 code units.
type structuredMention struct {
	uid    string
	name   string
	offset int // UTF-16 offset of '@' in source
	length int // UTF-16 length of the full @[uid:name] match
}

// structuredMentionRE mirrors STRUCTURED_MENTION_PATTERN in mention-utils.ts:
//
//	@\[([\w.\-]+):([^\]\n]+)\]
//
// uid charset [\w.\-]+ covers all known Octo uid formats; name is anything but a
// closing bracket or newline.
var structuredMentionRE = regexp.MustCompile(`@\[([\w.\-]+):([^\]\n]+)\]`)

// nameCharRE mirrors NAME_CHAR_RE in mention-utils.ts: a single valid @name
// character (letters, digits, underscore, Latin-extended/accented, CJK, Kana,
// Hangul, dot, hyphen). Used for plain-@name capture and boundary checks.
var nameCharRE = regexp.MustCompile(`[\w\x{00C0}-\x{024F}\x{4e00}-\x{9fff}\x{3040}-\x{30FF}\x{AC00}-\x{D7AF}.\-]`)

// utf16Width returns the number of UTF-16 code units a rune occupies (1 for the
// BMP, 2 for a surrogate pair). Matches JS string indexing.
func utf16Width(r rune) int {
	if r > 0xFFFF {
		return 2
	}
	return 1
}

// parseStructuredMentions extracts @[uid:name] mentions, computing each match's
// offset/length in UTF-16 code units. Ports parseStructuredMentions.
func parseStructuredMentions(text string) []structuredMention {
	idx := structuredMentionRE.FindAllStringSubmatchIndex(text, -1)
	if len(idx) == 0 {
		return nil
	}
	// Prefix-sum UTF-16 offsets indexed by byte position so each match's byte
	// start/length can be translated into UTF-16 units.
	out := make([]structuredMention, 0, len(idx))
	for _, m := range idx {
		byteStart, byteEnd := m[0], m[1]
		out = append(out, structuredMention{
			uid:    text[m[2]:m[3]],
			name:   text[m[4]:m[5]],
			offset: utf16OffsetAt(text, byteStart),
			length: utf16Len(text[byteStart:byteEnd]),
		})
	}
	return out
}

// utf16OffsetAt returns the UTF-16 code-unit offset of the byte position pos.
func utf16OffsetAt(text string, pos int) int {
	return utf16Len(text[:pos])
}

// utf16Len returns the length of s in UTF-16 code units (JS string.length).
func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// convertResult is the output of convertStructuredMentions.
type convertResult struct {
	content  string
	entities []MentionEntity
	uids     []string
}

// convertStructuredMentions replaces each @[uid:name] with @name and produces
// entities pointing at the @name positions in the OUTPUT (UTF-16 offsets). Ports
// convertStructuredMentions — incremental build, no indexOf rescans.
func convertStructuredMentions(text string, mentions []structuredMention) convertResult {
	sorted := make([]structuredMention, len(mentions))
	copy(sorted, mentions)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].offset < sorted[j].offset })

	// Walk in UTF-16-offset order over a []rune view so we can copy gaps by
	// code-unit position. Track the source cursor in UTF-16 units.
	runes := []rune(text)
	// Build a rune-index ↔ utf16-offset map lazily as we scan.
	var content strings.Builder
	entities := make([]MentionEntity, 0, len(sorted))
	uids := make([]string, 0, len(sorted))

	out16 := 0 // UTF-16 length of content written so far
	src16 := 0 // UTF-16 offset of the next unconsumed source rune
	ri := 0    // rune index aligned with src16

	for _, m := range sorted {
		// Copy gap [src16, m.offset).
		for ri < len(runes) && src16 < m.offset {
			r := runes[ri]
			content.WriteRune(r)
			w := utf16Width(r)
			out16 += w
			src16 += w
			ri++
		}
		replacement := "@" + m.name
		newOffset := out16
		content.WriteString(replacement)
		repLen := utf16Len(replacement)
		out16 += repLen
		entities = append(entities, MentionEntity{UID: m.uid, Offset: newOffset, Length: repLen})
		uids = append(uids, m.uid)
		// Advance source cursor past the consumed @[uid:name].
		target := m.offset + m.length
		for ri < len(runes) && src16 < target {
			w := utf16Width(runes[ri])
			src16 += w
			ri++
		}
	}
	// Copy the tail.
	for ri < len(runes) {
		content.WriteRune(runes[ri])
		ri++
	}

	return convertResult{content: content.String(), entities: entities, uids: uids}
}

// resolveResult is the output of resolveMentions.
type resolveResult struct {
	finalContent   string
	mentionUids    []string
	mentionEntries []MentionEntity
	mentionAll     bool
}

// mentionAllRE matches the @all/@所有人 token body WITHOUT boundaries (RE2 has no
// lookbehind/lookahead); boundaries are checked manually in detectMentionAll.
var mentionAllRE = regexp.MustCompile(`(?i)^(all|所有人)`)

// detectMentionAll emulates the mentionAll regex in resolveMentions:
//
//	(?:^|(?<=\s))@(?:all|所有人)(?!NAME_CHAR) case-insensitive
//
// The lead must be line start or whitespace; the trailing char must be a
// non-name char (or EOS). Notably the trailing boundary excludes '.' and '-'
// (which ARE name chars), so @all.x / @all-foo do NOT broadcast.
func detectMentionAll(content string) bool {
	runes := []rune(content)
	for i := 0; i < len(runes); i++ {
		if isMentionAllAt(runes, i) {
			return true
		}
	}
	return false
}

func isMentionAllAt(runes []rune, i int) bool {
	if runes[i] != '@' {
		return false
	}
	if !hasMentionAllLeadBoundary(runes, i) {
		return false
	}
	after := string(runes[i+1:])
	loc := mentionAllRE.FindStringSubmatchIndex(after)
	if loc == nil {
		return false
	}
	// token = all|所有人; trailing char check.
	tokenRunes := len([]rune(after[loc[2]:loc[3]]))
	rest := runes[i+1+tokenRunes:]
	return hasMentionAllTrailBoundary(rest)
}

func hasMentionAllLeadBoundary(runes []rune, i int) bool {
	if i == 0 {
		return true
	}
	return isASCIISpace(runes[i-1])
}

func hasMentionAllTrailBoundary(rest []rune) bool {
	if len(rest) == 0 {
		return true
	}
	return !nameCharRE.MatchString(string(rest[0]))
}

// resolveMentions runs the structured and plain pipelines, deduplicates entities
// by offset, and detects @all/@所有人. isValidUid (optional; nil = trust all)
// downgrades a structured mention whose uid is not a real member to plain text
// (the @name stays in finalContent, the entity/uid is dropped). Ports
// resolveMentions.
func resolveMentions(content string, memberMap map[string]string, isValidUid func(string) bool) resolveResult {
	finalContent := content
	var entities []MentionEntity

	// v2: @[uid:name] → @name + entities.
	structured := parseStructuredMentions(finalContent)
	if len(structured) > 0 {
		converted := convertStructuredMentions(finalContent, structured)
		finalContent = converted.content
		if isValidUid != nil {
			for _, e := range converted.entities {
				if isValidUid(e.UID) {
					entities = append(entities, e)
				}
			}
		} else {
			entities = append(entities, converted.entities...)
		}
	}

	// v1: @name fallback via memberMap, skipping offsets already covered by v2.
	if len(memberMap) > 0 {
		fallback := buildEntitiesFromFallback(finalContent, memberMap)
		existing := make(map[int]bool, len(entities))
		for _, e := range entities {
			existing[e.Offset] = true
		}
		for _, e := range fallback.entities {
			if !existing[e.Offset] {
				entities = append(entities, e)
			}
		}
	}

	sort.SliceStable(entities, func(i, j int) bool { return entities[i].Offset < entities[j].Offset })
	uids := make([]string, 0, len(entities))
	for _, e := range entities {
		uids = append(uids, e.UID)
	}

	return resolveResult{
		finalContent:   finalContent,
		mentionUids:    uids,
		mentionEntries: entities,
		mentionAll:     detectMentionAll(finalContent),
	}
}
