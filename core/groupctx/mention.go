package groupctx

import "regexp"

// mentionRE matches @name where name is ASCII word chars + CJK unified, CJK
// ext-A, Hangul, Hiragana, Katakana. RE2 \w is ASCII only, so we spell the
// ASCII class explicitly and add the Unicode ranges (matching group-context.ts).
var mentionRE = regexp.MustCompile(`@([0-9A-Za-z_\x{4e00}-\x{9fff}\x{3400}-\x{4dbf}\x{ac00}-\x{d7af}\x{3040}-\x{309f}\x{30a0}-\x{30ff}]+)`)

// mentionTrailingPunct is stripped from the right of a matched name.
const mentionTrailingPunct = ",.!?;:，。！？；：、)]"
