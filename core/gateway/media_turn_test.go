package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/store"
)

func TestMediaAuth_PerHopScoping(t *testing.T) {
	var sawAuthOnFirst, sawAuthOnSecond string
	// Second server is a "different host" — token must NOT travel here.
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthOnSecond = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("img"))
	}))
	defer second.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthOnFirst = r.Header.Get("Authorization")
		http.Redirect(w, r, second.URL+"/final", http.StatusFound)
	}))
	defer first.Close()

	// Auth hook authorizes only the first host.
	g := &Gateway{
		assertPublic: func(context.Context, string) error { return nil },
		mediaClient:  loopbackMediaClient(),
		mediaAuth: func(u string) string {
			if strings.HasPrefix(u, first.URL) {
				return "Bearer secret-token"
			}
			return ""
		},
	}
	atts := []router.Attachment{{Kind: router.AttachmentImage, URL: first.URL + "/img"}}
	g.materializeAttachments(context.Background(), t.TempDir(), atts)

	if sawAuthOnFirst != "Bearer secret-token" {
		t.Fatalf("first hop should carry token, got %q", sawAuthOnFirst)
	}
	if sawAuthOnSecond != "" {
		t.Fatalf("cross-host redirect must drop token, got %q", sawAuthOnSecond)
	}
}

// TestRunTurn_MediaHintTurnLocal proves the materialized hint reaches the
// driver prompt for THIS turn but never the stored history (which keeps the
// original text only) — the turn-local invariant from inbound.ts.
func TestRunTurn_MediaHintTurnLocal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("imgdata"))
	}))
	defer srv.Close()

	st := newTestStore(t)
	drv := &fakeDriver{threadID: "thr-m", reply: "ok"}
	cwdBase := t.TempDir()
	gw := New(drv, st, router.New(router.Config{MaxPerMinute: 100}), newCaptureSink()).
		WithSandbox(cwdBase, "")
	gw.assertPublic = func(context.Context, string) error { return nil } // allow httptest loopback
	gw.mediaClient = loopbackMediaClient()                               // bypass dial guard for loopback httptest

	msg := router.InboundMessage{
		ChannelType: router.ChannelDM,
		FromUID:     "u1",
		FromName:    "alice",
		Text:        "look at this",
		Attachments: []router.Attachment{{Kind: router.AttachmentImage, URL: srv.URL + "/a.png"}},
	}
	if _, err := gw.Handle(context.Background(), msg); err != nil {
		t.Fatalf("handle: %v", err)
	}

	// Prompt carries the Read hint.
	if len(drv.requests) != 1 {
		t.Fatalf("want 1 request, got %d", len(drv.requests))
	}
	prompt := drv.requests[0].Prompt
	if !strings.Contains(prompt, "look at this") || !strings.Contains(prompt, "已下载图片到本地") {
		t.Fatalf("prompt missing text or hint: %q", prompt)
	}
	// History stored the original text ONLY — no hint, no local path.
	msgs, _ := st.RecentMessages("u1", 10)
	if len(msgs) == 0 || msgs[0].Role != store.RoleUser {
		t.Fatalf("history wrong: %+v", msgs)
	}
	if msgs[0].Content != "look at this" {
		t.Fatalf("history must store original text only, got %q", msgs[0].Content)
	}
}
