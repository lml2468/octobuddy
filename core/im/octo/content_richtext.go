package octo

import "strings"

// --- RichText (type 14) ---

// richTextBlock is one normalized RichText content block.
type richTextBlock struct {
	Type string
	Text string
	URL  string
}

// resolveRichTextContent expands a RichText payload into {mediaURLs, text}
// (inbound.ts resolveRichTextContent). Prefers the server-authoritative top
// `plain`; else assembles from blocks (text → text, image → placeholder).
// Output text is byte-capped and mediaURLs is count-capped.
func resolveRichTextContent(content any, plain string, apiURL string) ([]string, string) {
	blocks := normalizeRichTextBlocks(content)
	mediaURLs := []string{}
	for _, blk := range blocks {
		if blk.Type == richTextBlockImage && blk.URL != "" {
			full := buildMediaURL(blk.URL, apiURL)
			if full != "" {
				mediaURLs = append(mediaURLs, full)
			}
			if len(mediaURLs) >= RichTextMaxMediaURLs {
				break
			}
		}
	}
	raw := plain
	if strings.TrimSpace(raw) == "" {
		raw = buildRichTextPlain(blocks)
	}
	text := truncateByBytes(raw, RichTextMaxOutputBytes, "\n[RichText truncated]")
	return mediaURLs, text
}

// normalizeRichTextBlocks coerces a RichText `content` field into blocks: an
// array → its object elements (capped at RichTextMaxBlocks); a bare string →
// one text block (back-compat); anything else → empty (inbound.ts
// normalizeRichTextBlocks).
func normalizeRichTextBlocks(content any) []richTextBlock {
	switch c := content.(type) {
	case []any:
		out := make([]richTextBlock, 0, len(c))
		for _, el := range c {
			m, ok := el.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, richTextBlock{
				Type: asString(m["type"]),
				Text: asString(m["text"]),
				URL:  asString(m["url"]),
			})
			if len(out) >= RichTextMaxBlocks {
				break
			}
		}
		return out
	case string:
		if c != "" {
			return []richTextBlock{{Type: richTextBlockText, Text: c}}
		}
	}
	return nil
}

// asString returns v when it is a string, else "" — guards against a malformed
// non-string `text`/`url`/`type` rendering as garbage.
func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// buildRichTextPlain assembles plain text from blocks: text → text, image →
// placeholder, unknown-with-text → text (inbound.ts buildRichTextPlain).
func buildRichTextPlain(blocks []richTextBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		switch {
		case blk.Type == richTextBlockImage:
			b.WriteString(richTextImagePlacehold)
		case blk.Type == richTextBlockText:
			b.WriteString(blk.Text)
		case blk.Text != "":
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}
