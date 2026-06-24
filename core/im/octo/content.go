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
	"strings"

	"github.com/lml2468/octobuddy/core/safety"
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

// --- Core resolver ---

// ResolveContent renders an inbound payload to LLM-friendly text plus any
// embedded media URLs (inbound.ts resolveContent). Text is never empty for a
// supported type, so the connector can stop dropping non-text payloads.
func ResolveContent(p MessagePayload, apiURL string) ResolvedContent {
	if marker, ok := urlMarkerForMessageType(p.Type); ok {
		return resolveURLMarkerContent(marker, p.URL, apiURL)
	}
	switch p.Type {
	case MsgText:
		return ResolvedContent{Text: p.Content}

	case MsgFile:
		return resolveFileContent(p, apiURL)

	case MsgLocation:
		return resolveLocationContent(p)

	case MsgCard:
		return resolveCardContent(p)

	case MsgMultipleForward:
		return ResolvedContent{Text: resolveMultipleForwardText(p.Users, p.Msgs, apiURL, 0)}

	case MsgRichText:
		urls, text := resolveRichTextContent(p.RichContent, p.Plain, apiURL)
		return ResolvedContent{Text: text, MediaURLs: urls}

	default:
		return resolveUnknownContent(p)
	}
}

func urlMarkerForMessageType(t MessageType) (string, bool) {
	switch t {
	case MsgImage:
		return "[图片]", true
	case MsgGIF:
		return "[GIF]", true
	case MsgVoice:
		// The model receives the URL as a marker; transcription is out of scope.
		return "[语音消息]", true
	case MsgVideo:
		return "[视频]", true
	default:
		return "", false
	}
}

func resolveURLMarkerContent(marker, rawURL, apiURL string) ResolvedContent {
	return ResolvedContent{Text: withURL(marker, buildMediaURL(rawURL, apiURL))}
}

func resolveFileContent(p MessagePayload, apiURL string) ResolvedContent {
	// payload.name is user-controlled — sanitize before the `[文件: …]` label.
	name := safety.SanitizeDisplayName(p.Name, "未知文件")
	return ResolvedContent{Text: withURL("[文件: "+name+"]", buildMediaURL(p.URL, apiURL))}
}

func resolveLocationContent(p MessagePayload) ResolvedContent {
	lat, latOK := toFiniteCoord(firstNonNil(p.Latitude, p.Lat))
	lng, lngOK := toFiniteCoord(firstNonNil(p.Longitude, p.Lng, p.Lon))
	if latOK && lngOK {
		return ResolvedContent{Text: "[位置信息: " + formatCoord(lat) + "," + formatCoord(lng) + "]"}
	}
	return ResolvedContent{Text: "[位置信息]"}
}

func resolveCardContent(p MessagePayload) ResolvedContent {
	// name + uid are user-controlled — sanitize both for the `[名片: …]` label.
	name := safety.SanitizeDisplayName(p.Name, "未知")
	uid := safety.SanitizeDisplayName(p.UID, "")
	if uid != "" {
		return ResolvedContent{Text: "[名片: " + name + " (" + uid + ")]"}
	}
	return ResolvedContent{Text: "[名片: " + name + "]"}
}

func resolveUnknownContent(p MessagePayload) ResolvedContent {
	if p.Content != "" {
		return ResolvedContent{Text: p.Content}
	}
	if p.URL != "" {
		return ResolvedContent{Text: p.URL}
	}
	return ResolvedContent{Text: "[消息]"}
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
