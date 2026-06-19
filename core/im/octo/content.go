package octo

// Inbound payload rendering — converts a decrypted MessagePayload into the
// LLM-friendly text the agent actually reads. Ported from cc-channel-octo's
// src/inbound.ts (resolveContent / resolveRichTextContent /
// resolveMultipleForwardText / buildMediaUrl) and cross-checked against
// openclaw-channel-octo's src/inbound.ts.
//
// Every supported MessageType renders to a non-empty text representation so the
// gateway no longer silently drops non-text payloads. The functions here are
// pure (no I/O): the connector calls them from onInbound. File download /
// inlining (G2) is intentionally out of scope for this unit — a File payload
// renders to its `[文件: <name>]` marker plus URL.
//
// SECURITY: all user-controlled names/bodies that land inside a prompt label
// (`[文件: …]`, `[名片: …]`, `<sender>: <body>`) are routed through
// core/safety — SanitizeDisplayName for names that become labels,
// SanitizePromptBody for forwarded leaf bodies — so a payload can never forge a
// section marker or role label (prompt injection). This mirrors inbound.ts's
// per-field sanitizeDisplayName/sanitizePromptBody calls.

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/lml2468/xclaw/core/safety"
)

// RichText block-type tags and the inline-image placeholder (types.ts
// RICH_TEXT_BLOCK_TEXT / RICH_TEXT_BLOCK_IMAGE / RICH_TEXT_IMAGE_PLACEHOLDER).
const (
	richTextBlockText      = "text"
	richTextBlockImage     = "image"
	richTextImagePlacehold = "[图片]"
)

// Per-payload parse budgets (inbound.ts C1 / Stage 6). Independent of and
// strictly tighter than the system-prompt-wide cap: they stop a single
// malicious payload from spending the whole prompt budget or OOMing the parser.
const (
	// RichTextMaxBlocks caps blocks parsed from a RichText payload.
	RichTextMaxBlocks = 50
	// RichTextMaxMediaURLs caps image URLs extracted from a RichText payload.
	RichTextMaxMediaURLs = 20
	// RichTextMaxOutputBytes caps rendered text from a single RichText payload.
	RichTextMaxOutputBytes = 32 * 1024

	// MultipleForwardMaxDepth caps MultipleForward recursion (top level = 0).
	MultipleForwardMaxDepth = 3
	// MultipleForwardMaxMessages caps inner messages rendered per level.
	MultipleForwardMaxMessages = 50
	// MultipleForwardMaxOutputBytes caps rendered transcript per payload.
	MultipleForwardMaxOutputBytes = 8 * 1024

	// QuotedBodyMaxBytes caps the quoted-reply body prepended to a turn
	// (inbound.ts quotePrefix has no explicit cap; we add one to bound the
	// untrusted quote, matching the unit spec's 4KB).
	QuotedBodyMaxBytes = 4 * 1024
)

// ResolvedContent is the rendering of one inbound payload. Text is always
// present (never empty for a supported type); MediaURLs carries every embedded
// RichText image URL in order (empty for single-media / text types).
type ResolvedContent struct {
	Text      string
	MediaURLs []string
}

// buildMediaURL resolves a relative storage path against the bot API base,
// hardened against absolute-URL smuggling and path traversal (inbound.ts
// buildMediaUrl, S1 + P1.2). Returns "" when the input is unsafe or empty so a
// malicious payload.url can never be fetched with the bot's Authorization
// header (token-leak chokepoint) nor forge a marker line.
//
// xclaw has no separate CDN host config, so only the apiUrl host is allowed for
// absolute URLs; the relative-path branch is preserved verbatim.
func buildMediaURL(relURL, apiURL string) string {
	if relURL == "" {
		return ""
	}
	// Backslashes are a Windows-style traversal vector once normalized.
	if strings.Contains(relURL, "\\") {
		return ""
	}
	// Scheme-relative URL (`//attacker.com/path`).
	if strings.HasPrefix(relURL, "//") {
		return ""
	}

	if strings.HasPrefix(relURL, "http://") || strings.HasPrefix(relURL, "https://") {
		if apiURL == "" {
			return ""
		}
		target, err := url.Parse(relURL)
		if err != nil {
			return ""
		}
		base, err := url.Parse(apiURL)
		if err != nil {
			return ""
		}
		if !strings.EqualFold(target.Host, base.Host) {
			return ""
		}
		// Same host: reject a protocol downgrade/upgrade mismatch.
		if !strings.EqualFold(target.Scheme, base.Scheme) {
			return ""
		}
		if target.Scheme != "http" && target.Scheme != "https" {
			return ""
		}
		return relURL
	}

	// Relative path — strip the /file/ or /file/preview/ prefix, then enforce
	// no traversal. The percent-encoded-dot and %2F defenses mirror inbound.ts:
	// some servers decode them server-side and resolve dot-segments, escaping
	// the /file/ sandbox.
	storagePath := relURL
	switch {
	case strings.HasPrefix(storagePath, "file/preview/"):
		storagePath = storagePath[len("file/preview/"):]
	case strings.HasPrefix(storagePath, "file/"):
		storagePath = storagePath[len("file/"):]
	}
	for _, seg := range strings.Split(storagePath, "/") {
		if seg == ".." || seg == "." {
			return ""
		}
	}
	if strings.HasPrefix(storagePath, "/") {
		return ""
	}
	lower := strings.ToLower(storagePath)
	if strings.Contains(lower, "%2f") {
		return ""
	}
	if strings.Contains(lower, "%2e") {
		return ""
	}
	// Reject an encoded percent (%25…) too: a downstream store that double-decodes
	// could turn %252e back into %2e and then "." — a traversal the single-decode
	// checks above would miss (L21).
	if strings.Contains(lower, "%25") {
		return ""
	}

	baseURL := strings.TrimRight(apiURL, "/")
	candidate := baseURL + "/file/" + storagePath

	// WHATWG-canonical sandbox check: after normalization the path must still
	// be under /file/.
	normalized, err := url.Parse(candidate)
	if err != nil {
		return ""
	}
	if !strings.HasPrefix(normalized.Path, "/file/") {
		return ""
	}
	return candidate
}

// toFiniteCoord coerces a user-supplied coordinate to a finite number, or
// reports ok=false (inbound.ts toFiniteCoord). Accepts only a real number
// (json float64) or a numeric string — rejects nil/bool/object so a non-numeric
// string can't forge a label and a nil can't render a bogus 0.
func toFiniteCoord(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		if isFinite(t) {
			return t, true
		}
	case int:
		return float64(t), true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		n, err := strconv.ParseFloat(s, 64)
		if err == nil && isFinite(n) {
			return n, true
		}
	}
	return 0, false
}

// isFinite reports whether f is a real finite number (not NaN or ±Inf).
func isFinite(f float64) bool {
	return f == f && f-f == 0
}

// formatCoord renders a coordinate without a trailing ".0" so an integer-valued
// float matches the JS `${lat}` template (which prints "31", not "31.0").
func formatCoord(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// truncateByBytes truncates s to at most maxBytes UTF-8 bytes on a rune
// boundary, appending marker when it had to cut (inbound.ts truncateByBytes).
func truncateByBytes(s string, maxBytes int, marker string) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	// Walk back to a rune boundary so we never split a multi-byte rune.
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}

// utf8RuneStart reports whether b is the first byte of a UTF-8 rune (i.e. not a
// 0b10xxxxxx continuation byte).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

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

// --- MultipleForward (type 11) ---

// resolveInnerMessageText renders one forward child to text (inbound.ts
// resolveInnerMessageText). Leaf bodies are NOT escaped here — the caller
// sanitizes them once (see resolveMultipleForwardText).
func resolveInnerMessageText(p forwardPayload, apiURL string) string {
	full := buildMediaURL(p.URL, apiURL)
	switch MessageType(p.Type) {
	case MsgText:
		return p.Content
	case MsgImage:
		return withURL("[图片]", full)
	case MsgGIF:
		return withURL("[GIF]", full)
	case MsgVoice:
		return withURL("[语音]", full)
	case MsgVideo:
		return withURL("[视频]", full)
	case MsgLocation:
		return "[位置信息]"
	case MsgCard:
		return "[名片]"
	case MsgFile:
		// payload.name is user-controlled — sanitize before it enters the label.
		label := "[文件]"
		if p.Name != "" {
			if safe := safety.SanitizeDisplayName(p.Name, ""); safe != "" {
				label = "[文件: " + safe + "]"
			}
		}
		return withURL(label, full)
	case MsgMultipleForward:
		return "[合并转发]"
	case MsgRichText:
		_, text := resolveRichTextContent(p.RichContent, p.Plain, apiURL)
		if text == "" {
			return "[图文消息]"
		}
		return text
	default:
		if p.Content != "" {
			return p.Content
		}
		return "[消息]"
	}
}

// withURL appends "\n<url>" to label when url is non-empty (the recurring
// `fullUrl ? ${label}\n${url} : label` idiom in inbound.ts).
func withURL(label, u string) string {
	if u == "" {
		return label
	}
	return label + "\n" + u
}

// resolveMultipleForwardText expands a MultipleForward payload into a readable
// transcript, bounded by depth/message/byte caps (inbound.ts
// resolveMultipleForwardText). depth is hop-counted (top level = 0).
//
// SECURITY: u.name AND u.uid are user-controlled; both are sanitized for the
// `<name>: ` label (passing raw uid as the fallback would re-introduce the
// injection when name collapses to empty). Each leaf BODY is run through
// SanitizePromptBody so a forwarded body can't forge a turn boundary; nested
// transcripts are already escaped by their own recursion.
func resolveMultipleForwardText(users []forwardUser, msgs []forwardMessage, apiURL string, depth int) string {
	if depth >= MultipleForwardMaxDepth {
		return "[合并转发: 嵌套已截断]"
	}
	capped := msgs
	truncatedCount := 0
	if len(capped) > MultipleForwardMaxMessages {
		truncatedCount = len(capped) - MultipleForwardMaxMessages
		capped = capped[:MultipleForwardMaxMessages]
	}

	userMap := make(map[string]string, len(users))
	for _, u := range users {
		if u.UID != "" && u.Name != "" {
			safe := safety.SanitizeDisplayName(u.Name, "")
			if safe == "" {
				safe = safety.SanitizeDisplayName(u.UID, "unknown")
			}
			userMap[u.UID] = safe
		}
	}

	lines := []string{"[合并转发: 聊天记录]"}
	for _, m := range capped {
		senderName, ok := userMap[m.FromUID]
		if !ok {
			senderName = safety.SanitizeDisplayName(m.FromUID, "unknown")
		}
		if MessageType(m.Payload.Type) == MsgMultipleForward {
			nested := resolveMultipleForwardText(m.Payload.Users, m.Payload.Msgs, apiURL, depth+1)
			lines = append(lines, senderName+": [合并转发]", nested)
			continue
		}
		inner := safety.SanitizePromptBody(resolveInnerMessageText(m.Payload, apiURL))
		lines = append(lines, senderName+": "+inner)
	}
	if truncatedCount > 0 {
		lines = append(lines, "[合并转发: 还有 "+strconv.Itoa(truncatedCount)+" 条消息未展示]")
	}
	out := strings.Join(lines, "\n")
	return truncateByBytes(out, MultipleForwardMaxOutputBytes, "\n[合并转发: 输出已截断]")
}

// --- Core resolver ---

// ResolveContent renders an inbound payload to LLM-friendly text plus any
// embedded media URLs (inbound.ts resolveContent). Text is never empty for a
// supported type, so the connector can stop dropping non-text payloads.
func ResolveContent(p MessagePayload, apiURL string) ResolvedContent {
	switch p.Type {
	case MsgText:
		return ResolvedContent{Text: p.Content}

	case MsgImage:
		return ResolvedContent{Text: withURL("[图片]", buildMediaURL(p.URL, apiURL))}

	case MsgGIF:
		return ResolvedContent{Text: withURL("[GIF]", buildMediaURL(p.URL, apiURL))}

	case MsgVoice:
		// The model receives the URL as a marker; transcription is out of scope.
		return ResolvedContent{Text: withURL("[语音消息]", buildMediaURL(p.URL, apiURL))}

	case MsgVideo:
		return ResolvedContent{Text: withURL("[视频]", buildMediaURL(p.URL, apiURL))}

	case MsgFile:
		// payload.name is user-controlled — sanitize before the `[文件: …]` label.
		name := safety.SanitizeDisplayName(p.Name, "未知文件")
		return ResolvedContent{Text: withURL("[文件: "+name+"]", buildMediaURL(p.URL, apiURL))}

	case MsgLocation:
		lat, latOK := toFiniteCoord(firstNonNil(p.Latitude, p.Lat))
		lng, lngOK := toFiniteCoord(firstNonNil(p.Longitude, p.Lng, p.Lon))
		if latOK && lngOK {
			return ResolvedContent{Text: "[位置信息: " + formatCoord(lat) + "," + formatCoord(lng) + "]"}
		}
		return ResolvedContent{Text: "[位置信息]"}

	case MsgCard:
		// name + uid are user-controlled — sanitize both for the `[名片: …]` label.
		name := safety.SanitizeDisplayName(p.Name, "未知")
		uid := safety.SanitizeDisplayName(p.UID, "")
		if uid != "" {
			return ResolvedContent{Text: "[名片: " + name + " (" + uid + ")]"}
		}
		return ResolvedContent{Text: "[名片: " + name + "]"}

	case MsgMultipleForward:
		return ResolvedContent{Text: resolveMultipleForwardText(p.Users, p.Msgs, apiURL, 0)}

	case MsgRichText:
		urls, text := resolveRichTextContent(p.RichContent, p.Plain, apiURL)
		return ResolvedContent{Text: text, MediaURLs: urls}

	default:
		if p.Content != "" {
			return ResolvedContent{Text: p.Content}
		}
		if p.URL != "" {
			return ResolvedContent{Text: p.URL}
		}
		return ResolvedContent{Text: "[消息]"}
	}
}

// firstNonNil returns the first non-nil arg (the `a ?? b ?? c` coalescing used
// for lat/lng aliases in inbound.ts).
func firstNonNil(vs ...any) any {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

// resolveQuotePrefix builds the `[Quoted message from <name>]: <body>\n---\n`
// prefix from a reply payload, or "" when there is no quotable body (inbound.ts
// quotePrefix block in handleInboundMessage). Name and body are both untrusted
// and sanitized; the body is byte-capped.
//
// Injected into the CURRENT turn's text only — never stored history.
func resolveQuotePrefix(reply *ReplyPayload, apiURL string) string {
	if reply == nil {
		return ""
	}
	var body string
	if reply.Payload != nil {
		// RichText carries `content` as a block array, not a string; route
		// through ResolveContent so it never interpolates a raw object.
		body = ResolveContent(*reply.Payload, apiURL).Text
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	from := reply.FromName
	if from == "" {
		from = reply.FromUID
	}
	safeName := safety.SanitizeDisplayName(from, "unknown")
	safeBody := safety.SanitizePromptBody(truncateByBytes(body, QuotedBodyMaxBytes, "…"))
	return "[Quoted message from " + safeName + "]: " + safeBody + "\n---\n"
}
