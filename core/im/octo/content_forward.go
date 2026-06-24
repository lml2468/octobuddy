package octo

import (
	"strconv"
	"strings"

	"github.com/lml2468/octobuddy/core/safety"
)

// --- MultipleForward (type 11) ---

// resolveInnerMessageText renders one forward child to text (inbound.ts
// resolveInnerMessageText). Leaf bodies are NOT escaped here — the caller
// sanitizes them once (see resolveMultipleForwardText).
func resolveInnerMessageText(p forwardPayload, apiURL string) string {
	if text, ok := simpleInnerMessageText(p, apiURL); ok {
		return text
	}
	full := buildMediaURL(p.URL, apiURL)
	switch MessageType(p.Type) {
	case MsgText:
		return p.Content
	case MsgFile:
		// payload.name is user-controlled — sanitize before it enters the label.
		label := "[文件]"
		if p.Name != "" {
			if safe := safety.SanitizeDisplayName(p.Name, ""); safe != "" {
				label = "[文件: " + safe + "]"
			}
		}
		return withURL(label, full)
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

func simpleInnerMessageText(p forwardPayload, apiURL string) (string, bool) {
	full := buildMediaURL(p.URL, apiURL)
	switch MessageType(p.Type) {
	case MsgImage:
		return withURL("[图片]", full), true
	case MsgGIF:
		return withURL("[GIF]", full), true
	case MsgVoice:
		return withURL("[语音]", full), true
	case MsgVideo:
		return withURL("[视频]", full), true
	case MsgLocation:
		return "[位置信息]", true
	case MsgCard:
		return "[名片]", true
	case MsgMultipleForward:
		return "[合并转发]", true
	default:
		return "", false
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

	userMap := forwardUserNameMap(users)

	lines := []string{"[合并转发: 聊天记录]"}
	for _, m := range capped {
		senderName := forwardSenderName(userMap, m.FromUID)
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

func forwardUserNameMap(users []forwardUser) map[string]string {
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
	return userMap
}

func forwardSenderName(userMap map[string]string, uid string) string {
	if senderName, ok := userMap[uid]; ok {
		return senderName
	}
	return safety.SanitizeDisplayName(uid, "unknown")
}
