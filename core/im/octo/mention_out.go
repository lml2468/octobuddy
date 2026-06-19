package octo

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf16"
)

// Outbound @mention resolution, ported from cc-channel-octo's mention-utils.ts
// (and the deliver() splitting in stream-relay.ts).
//
// Two formats are recognized in agent output:
//   - v2 structured: @[uid:displayName] — precise, generated from the system
//     prompt; converted to a human-readable @displayName plus a MentionEntity.
//   - v1 plain: @name — resolved against the channel roster (displayName → uid).
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

// fallbackResult is the output of buildEntitiesFromFallback.
type fallbackResult struct {
	entities []MentionEntity
	uids     []string
}

// isMentionLeadBoundaryOK emulates MENTION_PATTERN's lookbehind
// `(?:^|(?<=\s|[^a-zA-Z0-9]))`: the '@' must be at line start or preceded by a
// whitespace / non-alphanumeric rune. prevRune is the rune immediately before
// '@' (utf8.RuneError-equivalent handling: callers pass a sentinel for start).
func isMentionLeadBoundaryOK(prev rune, atStart bool) bool {
	if atStart {
		return true
	}
	if prev == ' ' || prev == '\t' || prev == '\n' || prev == '\r' || prev == '\f' || prev == '\v' {
		return true
	}
	// Non-alphanumeric (ASCII a-zA-Z0-9 are the only blacklisted lead chars).
	isAlnum := (prev >= 'a' && prev <= 'z') || (prev >= 'A' && prev <= 'Z') || (prev >= '0' && prev <= '9')
	return !isAlnum
}

// tryLongestMemberMatch tries the longest displayName in sortedNames that the
// text starts with at the position just after '@' (afterAt = rune slice after
// '@'). Boundary: the char after the name must be a name-terminator. Ports
// tryLongestMemberMatch. Returns (name, uid, true) on success.
func tryLongestMemberMatch(afterAt []rune, memberMap map[string]string, sortedNames []string) (string, string, bool) {
	after := string(afterAt)
	for _, candidate := range sortedNames {
		if strings.HasPrefix(after, candidate) {
			rest := afterAt[len([]rune(candidate)):]
			if len(rest) == 0 || !nameCharRE.MatchString(string(rest[0])) {
				if uid, ok := memberMap[candidate]; ok {
					return candidate, uid, true
				}
			}
		}
	}
	return "", "", false
}

// buildEntitiesFromFallback resolves plain @name mentions against memberMap
// (displayName → uid), longest-prefix first, skipping @all / @所有人. Offsets are
// UTF-16 code units into content. Ports buildEntitiesFromFallback.
func buildEntitiesFromFallback(content string, memberMap map[string]string) fallbackResult {
	var res fallbackResult
	if len(memberMap) == 0 {
		return res
	}
	sortedNames := make([]string, 0, len(memberMap))
	for k := range memberMap {
		sortedNames = append(sortedNames, k)
	}
	// Longest first; ties broken lexicographically for determinism.
	sort.Slice(sortedNames, func(i, j int) bool {
		if len(sortedNames[i]) != len(sortedNames[j]) {
			return len(sortedNames[i]) > len(sortedNames[j])
		}
		return sortedNames[i] < sortedNames[j]
	})

	runes := []rune(content)
	off16 := 0 // UTF-16 offset of runes[i]
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r != '@' {
			off16 += utf16Width(r)
			continue
		}
		var prev rune
		atStart := i == 0
		if !atStart {
			prev = runes[i-1]
		}
		if !isMentionLeadBoundaryOK(prev, atStart) {
			off16 += utf16Width(r)
			continue
		}
		afterAt := runes[i+1:]
		// Determine the captured plain name (charset run after '@').
		nameLen := 0
		for nameLen < len(afterAt) && nameCharRE.MatchString(string(afterAt[nameLen])) {
			nameLen++
		}
		if nameLen == 0 {
			off16 += utf16Width(r)
			continue
		}
		name := string(afterAt[:nameLen])
		// Skip @all / @所有人 — handled by mentionAll.
		if strings.EqualFold(name, "all") || name == "所有人" {
			off16 += utf16Width(r)
			continue
		}

		matchedName := name
		uid, ok := "", false
		if mn, mu, found := tryLongestMemberMatch(afterAt, memberMap, sortedNames); found {
			matchedName, uid, ok = mn, mu, true
		} else if u, exists := memberMap[name]; exists {
			uid, ok = u, true
		}
		if !ok {
			off16 += utf16Width(r)
			continue
		}

		atName := "@" + matchedName
		atLen16 := utf16Len(atName)
		res.entities = append(res.entities, MentionEntity{UID: uid, Offset: off16, Length: atLen16})
		res.uids = append(res.uids, uid)

		// Advance past the full match (whole @matchedName), in both rune and
		// UTF-16 space, to avoid re-matching trailing chars of long names.
		matchedRunes := len([]rune(matchedName))
		// Move i to the last consumed rune (loop's i++ then steps past it).
		consumed := 1 + matchedRunes // '@' + name runes
		off16 += utf16Len(string(runes[i : i+consumed]))
		i += consumed - 1
	}
	return res
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
//	(?:^|(?<=\s))@(?:all|所有人)(?!NAME_CHAR)   case-insensitive
//
// The lead must be line start or whitespace; the trailing char must be a
// non-name char (or EOS). Notably the trailing boundary excludes '.' and '-'
// (which ARE name chars), so @all.x / @all-foo do NOT broadcast.
func detectMentionAll(content string) bool {
	runes := []rune(content)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '@' {
			continue
		}
		// Lead boundary: start, or preceded by whitespace.
		if i > 0 {
			p := runes[i-1]
			if !(p == ' ' || p == '\t' || p == '\n' || p == '\r' || p == '\f' || p == '\v') {
				continue
			}
		}
		after := string(runes[i+1:])
		loc := mentionAllRE.FindStringSubmatchIndex(after)
		if loc == nil {
			continue
		}
		// token = all|所有人; trailing char check.
		tokenRunes := len([]rune(after[loc[2]:loc[3]]))
		rest := runes[i+1+tokenRunes:]
		if len(rest) == 0 || !nameCharRE.MatchString(string(rest[0])) {
			return true
		}
	}
	return false
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

// protectedRange is a UTF-16 [start,end) span that splitMessage must not cut
// through (used to keep a resolved @name whole). Mirrors ProtectedRange in
// stream-relay.ts.
type protectedRange struct {
	start int
	end   int
}

// segment is one output chunk plus its UTF-16 start offset within the full text,
// so the connector can rebase global entity offsets to segment-local ones.
type segment struct {
	text  string
	start int // UTF-16 offset of this segment's first code unit in the full text
}

// adjustSplitForProtectedRanges mirrors stream-relay.ts: if splitAt lands
// strictly inside a protected range, pull back to the range start (move the
// protected unit whole to the next segment). Returns (-1, false) when pulling
// back would land at 0.
// adjustSplitForProtectedRanges moves a candidate split point off of any
// protected range it lands inside. If the range has room before it, split there;
// if the range starts at 0 (a single mention longer than maxUnits at the segment
// start), there is no earlier boundary, so split at the range END to keep the
// mention intact in one (over-long) segment rather than slicing through it —
// returning (rangeEnd, true). A mention can never realistically exceed maxUnits
// (SanitizeDisplayName caps names well under 3500 UTF-16 units), so this is a
// safety net, not a hot path.
func adjustSplitForProtectedRanges(splitAt int, ranges []protectedRange) (int, bool) {
	for _, r := range ranges {
		if splitAt > r.start && splitAt < r.end {
			if r.start > 0 {
				return r.start, true
			}
			return r.end, true
		}
	}
	return splitAt, true
}

// splitMessageProtected ports stream-relay.ts splitMessage: split text into
// segments of at most maxUnits UTF-16 code units, preferring paragraph (\n\n) >
// newline (\n) > space > hard cut, never cutting through a protected range, and
// guarding against splitting a surrogate pair. Offsets in `ranges` are UTF-16,
// global to the full text. Returns segments with their global UTF-16 starts.
func splitMessageProtected(text string, maxUnits int, ranges []protectedRange) []segment {
	if maxUnits < 1 {
		maxUnits = 1
	}
	units := utf16.Encode([]rune(text))
	if len(units) <= maxUnits {
		return []segment{{text: text, start: 0}}
	}

	var segs []segment
	remaining := units
	consumed := 0

	for len(remaining) > 0 {
		if len(remaining) <= maxUnits {
			segs = append(segs, segment{text: decodeUTF16(remaining), start: consumed})
			break
		}

		chunk := remaining[:maxUnits]
		// Translate global ranges to local (relative to consumed).
		var local []protectedRange
		for _, r := range ranges {
			if r.end > consumed && r.start < consumed+len(remaining) {
				local = append(local, protectedRange{start: r.start - consumed, end: r.end - consumed})
			}
		}

		splitAt := -1
		try := func(candidate int) bool {
			adj, ok := adjustSplitForProtectedRanges(candidate, local)
			if !ok || adj <= 0 || adj > maxUnits {
				return false
			}
			splitAt = adj
			return true
		}

		// 1. Paragraph break.
		if idx := lastIndexUnits(chunk, "\n\n"); idx > 0 {
			try(idx + 2)
		}
		// 2. Newline.
		if splitAt == -1 {
			if idx := lastIndexUnits(chunk, "\n"); idx > 0 {
				try(idx + 1)
			}
		}
		// 3. Space.
		if splitAt == -1 {
			if idx := lastIndexUnits(chunk, " "); idx > 0 {
				try(idx + 1)
			}
		}
		// 4. Hard cut — avoid surrogate split and protected ranges.
		if splitAt == -1 {
			splitAt = maxUnits
			if c := remaining[splitAt-1]; c >= 0xD800 && c <= 0xDBFF {
				splitAt--
			}
			if adj, ok := adjustSplitForProtectedRanges(splitAt, local); ok && adj > 0 {
				// adj < splitAt: a protected range with room before it → cut earlier.
				// adj > splitAt: a mention longer than maxUnits starting at 0 → cut at
				// its end so the whole mention stays in this (over-long) segment
				// instead of being sliced through.
				splitAt = adj
			}
		}

		// adj from a start-0 oversized mention can exceed the remaining length;
		// clamp so the slice below never panics.
		if splitAt > len(remaining) {
			splitAt = len(remaining)
		}
		segs = append(segs, segment{text: decodeUTF16(remaining[:splitAt]), start: consumed})
		remaining = remaining[splitAt:]
		consumed += splitAt
	}

	return segs
}

// lastIndexUnits returns the UTF-16 code-unit index of the last occurrence of
// the (BMP-only) substring sub within units, or -1. sub must contain no
// surrogate-pair characters (callers pass "\n\n", "\n", " ").
func lastIndexUnits(units []uint16, sub string) int {
	subUnits := utf16.Encode([]rune(sub))
	if len(subUnits) == 0 || len(subUnits) > len(units) {
		return -1
	}
	for i := len(units) - len(subUnits); i >= 0; i-- {
		match := true
		for j := range subUnits {
			if units[i+j] != subUnits[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// decodeUTF16 turns a UTF-16 code-unit slice back into a Go string.
func decodeUTF16(units []uint16) string {
	return string(utf16.Decode(units))
}
