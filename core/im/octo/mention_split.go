package octo

import "unicode/utf16"

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

		local := localProtectedRanges(ranges, consumed, len(remaining))
		splitAt := chooseProtectedSplit(remaining, maxUnits, local)
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

func localProtectedRanges(ranges []protectedRange, consumed, remainingLen int) []protectedRange {
	var local []protectedRange
	for _, r := range ranges {
		if r.end > consumed && r.start < consumed+remainingLen {
			local = append(local, protectedRange{start: r.start - consumed, end: r.end - consumed})
		}
	}
	return local
}

func chooseProtectedSplit(remaining []uint16, maxUnits int, local []protectedRange) int {
	chunk := remaining[:maxUnits]
	if splitAt, ok := preferredProtectedSplit(chunk, maxUnits, local); ok {
		return splitAt
	}
	return hardProtectedSplit(remaining, maxUnits, local)
}

func preferredProtectedSplit(chunk []uint16, maxUnits int, local []protectedRange) (int, bool) {
	for _, candidate := range []struct {
		substr string
		width  int
	}{
		{substr: "\n\n", width: 2},
		{substr: "\n", width: 1},
		{substr: " ", width: 1},
	} {
		if idx := lastIndexUnits(chunk, candidate.substr); idx > 0 {
			if splitAt, ok := tryProtectedSplit(idx+candidate.width, maxUnits, local); ok {
				return splitAt, true
			}
		}
	}
	return 0, false
}

func tryProtectedSplit(candidate, maxUnits int, local []protectedRange) (int, bool) {
	adj, ok := adjustSplitForProtectedRanges(candidate, local)
	if !ok || adj <= 0 || adj > maxUnits {
		return 0, false
	}
	return adj, true
}

func hardProtectedSplit(remaining []uint16, maxUnits int, local []protectedRange) int {
	splitAt := maxUnits
	// Avoid surrogate split.
	if c := remaining[splitAt-1]; c >= 0xD800 && c <= 0xDBFF {
		splitAt--
	}
	if adj, ok := adjustSplitForProtectedRanges(splitAt, local); ok && adj > 0 {
		// adj < splitAt: a protected range with room before it → cut earlier.
		// adj > splitAt: a mention longer than maxUnits starting at 0 → cut at
		// its end so the whole mention stays in this (over-long) segment instead
		// of being sliced through.
		splitAt = adj
	}
	return splitAt
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
