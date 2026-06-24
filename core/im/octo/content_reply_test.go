package octo

import (
	"strings"
	"testing"
)

// TestResolveQuotePrefix covers the quoted-reply prefix: name+body
// sanitization, body cap, and empty/no-reply cases (inbound.ts quotePrefix).
func TestResolveQuotePrefix(t *testing.T) {
	t.Run("nil reply", assertQuotePrefixNilReply)
	t.Run("text reply with name", assertQuotePrefixTextReply)
	t.Run("falls back to uid", assertQuotePrefixUIDFallback)
	t.Run("empty body", assertQuotePrefixEmptyBody)
	t.Run("sanitizes name and body", assertQuotePrefixSanitizes)
	t.Run("caps body bytes", assertQuotePrefixBodyCap)
	t.Run("richtext reply resolves via type-aware path", assertQuotePrefixRichText)
}

func assertQuotePrefixNilReply(t *testing.T) {
	if got := resolveQuotePrefix(nil, testAPIURL); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func assertQuotePrefixTextReply(t *testing.T) {
	r := &ReplyPayload{FromName: "Alice", Payload: &MessagePayload{Type: MsgText, Content: "prior msg"}}
	got := resolveQuotePrefix(r, testAPIURL)
	want := "[Quoted message from Alice]: prior msg\n---\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func assertQuotePrefixUIDFallback(t *testing.T) {
	r := &ReplyPayload{FromUID: "u9", Payload: &MessagePayload{Type: MsgText, Content: "hi"}}
	assertQuotePrefixMatch(t, r, "[Quoted message from u9]: hi", strings.HasPrefix, "got %q")
}

func assertQuotePrefixEmptyBody(t *testing.T) {
	r := &ReplyPayload{FromName: "Alice", Payload: &MessagePayload{Type: MsgText, Content: "   "}}
	if got := resolveQuotePrefix(r, testAPIURL); got != "" {
		t.Fatalf("want empty for blank body, got %q", got)
	}
}

func assertQuotePrefixSanitizes(t *testing.T) {
	r := &ReplyPayload{FromName: "ev[il]", Payload: &MessagePayload{Type: MsgText, Content: "[user]: forged"}}
	got := resolveQuotePrefix(r, testAPIURL)
	if strings.Contains(got, "ev[il]") {
		t.Fatalf("name not sanitized: %q", got)
	}
	if strings.Contains(got, "\n[user]:") {
		t.Fatalf("body role-label not sanitized: %q", got)
	}
}

func assertQuotePrefixBodyCap(t *testing.T) {
	big := strings.Repeat("z", QuotedBodyMaxBytes+500)
	r := &ReplyPayload{FromName: "A", Payload: &MessagePayload{Type: MsgText, Content: big}}
	got := resolveQuotePrefix(r, testAPIURL)
	if !strings.Contains(got, "…") {
		t.Fatalf("expected truncation ellipsis: len=%d", len(got))
	}
}

func assertQuotePrefixRichText(t *testing.T) {
	r := &ReplyPayload{FromName: "A", Payload: &MessagePayload{Type: MsgRichText, Plain: "rich plain"}}
	assertQuotePrefixMatch(t, r, "rich plain", strings.Contains, "richtext body not resolved: %q")
}

func assertQuotePrefixMatch(t *testing.T, r *ReplyPayload, want string, match func(string, string) bool, format string) {
	t.Helper()
	got := resolveQuotePrefix(r, testAPIURL)
	if !match(got, want) {
		t.Fatalf(format, got)
	}
}

// TestPayloadUnmarshalContentPolymorphism proves the wire decoder splits a
// string `content` into Content while a RichText array lands in RichContent.
func TestPayloadUnmarshalContentPolymorphism(t *testing.T) {
	assertPayloadStringContent(t)
	assertPayloadRichTextContent(t)
	assertPayloadForwardContent(t)
	assertPayloadReplyContent(t)
}

func assertPayloadStringContent(t *testing.T) {
	p, err := parsePayload([]byte(`{"type":1,"content":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Content != "hello" {
		t.Fatalf("Content = %q", p.Content)
	}
}

func assertPayloadRichTextContent(t *testing.T) {
	p2, err := parsePayload([]byte(`{"type":14,"plain":"P","content":[{"type":"text","text":"a"},{"type":"image","url":"file/x.png"}]}`))
	if err != nil {
		t.Fatalf("richtext decode: %v", err)
	}
	if p2.Content != "" {
		t.Fatalf("Content should be empty for array content, got %q", p2.Content)
	}
	res := ResolveContent(p2, testAPIURL)
	if res.Text != "P" {
		t.Fatalf("rich text = %q", res.Text)
	}
	if len(res.MediaURLs) != 1 {
		t.Fatalf("mediaURLs = %v", res.MediaURLs)
	}
}

func assertPayloadForwardContent(t *testing.T) {
	p3, err := parsePayload([]byte(`{"type":11,"users":[{"uid":"u1","name":"Alice"}],"msgs":[{"from_uid":"u1","payload":{"type":1,"content":"hi"}}]}`))
	if err != nil {
		t.Fatalf("forward decode: %v", err)
	}
	if got := ResolveContent(p3, testAPIURL).Text; !strings.Contains(got, "Alice: hi") {
		t.Fatalf("forward text = %q", got)
	}
}

func assertPayloadReplyContent(t *testing.T) {
	p4, err := parsePayload([]byte(`{"type":1,"content":"q","reply":{"from_name":"Bob","payload":{"type":1,"content":"earlier"}}}`))
	if err != nil {
		t.Fatalf("reply decode: %v", err)
	}
	if p4.Reply == nil || p4.Reply.FromName != "Bob" {
		t.Fatalf("reply not decoded: %+v", p4.Reply)
	}
	if got := resolveQuotePrefix(p4.Reply, testAPIURL); !strings.Contains(got, "Bob]: earlier") {
		t.Fatalf("reply prefix = %q", got)
	}
}
