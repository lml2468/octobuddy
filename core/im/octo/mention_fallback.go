package octo

import (
	"sort"
	"strings"
)

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
	// Non-alphanumeric (ASCII a-zA-Z0-9 are the only blacklisted lead chars).
	if isASCIISpace(prev) {
		return true
	}
	return !isASCIIAlnum(prev)
}

func isASCIISpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v'
}

func isASCIIAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
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
	sortedNames := sortedFallbackMemberNames(memberMap)
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
		matchedName, uid, ok := resolveFallbackMention(afterAt, memberMap, sortedNames)
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

func sortedFallbackMemberNames(memberMap map[string]string) []string {
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
	return sortedNames
}

func resolveFallbackMention(afterAt []rune, memberMap map[string]string, sortedNames []string) (string, string, bool) {
	nameLen := plainMentionNameLen(afterAt)
	if nameLen == 0 {
		return "", "", false
	}
	name := string(afterAt[:nameLen])
	// Skip @all / @所有人 — handled by mentionAll.
	if strings.EqualFold(name, "all") || name == "所有人" {
		return "", "", false
	}
	if matchedName, uid, found := tryLongestMemberMatch(afterAt, memberMap, sortedNames); found {
		return matchedName, uid, true
	}
	if uid, exists := memberMap[name]; exists {
		return name, uid, true
	}
	return "", "", false
}

func plainMentionNameLen(afterAt []rune) int {
	nameLen := 0
	for nameLen < len(afterAt) && nameCharRE.MatchString(string(afterAt[nameLen])) {
		nameLen++
	}
	return nameLen
}
