package octo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lml2468/octobuddy/core/safepath"
)

func TestMessageTypeIsSystem(t *testing.T) {
	cases := []struct {
		t        MessageType
		isSystem bool
	}{
		{MsgText, false},
		{MsgRichText, false},
		{MsgCMD, true},
		{MsgSystemMin, true},       // 1000 FriendApply
		{MessageType(1002), true},  // GroupMemberAdd
		{MessageType(1005), true},  // GroupUpdate
		{MessageType(1021), true},  // GroupMemberQuit
		{MsgSystemMax, true},       // 2000 Tip
		{MessageType(2001), false}, // above range
		{MessageType(98), false},   // SignalError (not CMD)
	}
	for _, c := range cases {
		if got := c.t.IsSystem(); got != c.isSystem {
			t.Errorf("type %d IsSystem=%v want %v", c.t, got, c.isSystem)
		}
	}
}

func TestIsControlEvent(t *testing.T) {
	// event envelope present → control event, even though Type is Text(1).
	p, err := parsePayload([]byte(`{"type":1,"content":"GROUP.md updated","event":{"type":"group_md_updated","version":3,"updated_by":"u1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !p.IsControlEvent() {
		t.Fatal("payload with event envelope should be a control event")
	}
	if p.Event.Type != EventGroupMdUpdated || p.Event.Version != 3 {
		t.Fatalf("event decoded wrong: %+v", p.Event)
	}

	// plain text → not a control event.
	p2, _ := parsePayload([]byte(`{"type":1,"content":"hi"}`))
	if p2.IsControlEvent() {
		t.Fatal("plain text must not be a control event")
	}

	// empty event.type → not a control event (defensive).
	p3, _ := parsePayload([]byte(`{"type":1,"event":{"version":1}}`))
	if p3.IsControlEvent() {
		t.Fatal("event with empty type must not count")
	}
}

func TestClassifyControlMessage(t *testing.T) {
	ev := func(typ string) MessagePayload { return MessagePayload{Type: MsgText, Event: &PayloadEvent{Type: typ}} }
	cases := []struct {
		name string
		p    MessagePayload
		want sysAction
	}{
		{"group md updated", ev(EventGroupMdUpdated), sysActionMirrorDoc},
		{"thread md updated", ev(EventThreadMdUpdated), sysActionMirrorDoc},
		{"group md deleted", ev(EventGroupMdDeleted), sysActionDeleteDoc},
		{"thread md deleted", ev(EventThreadMdDeleted), sysActionDeleteDoc},
		{"mention pref", ev(EventMentionPrefUpdate), sysActionIgnore},
		{"member add type", MessagePayload{Type: MessageType(1002)}, sysActionIgnore},
		{"group update type", MessagePayload{Type: MessageType(1005)}, sysActionIgnore},
		{"plain text", MessagePayload{Type: MsgText, Content: "hi"}, sysActionIgnore},
	}
	for _, tc := range cases {
		if got := classifyControlMessage(tc.p); got != tc.want {
			t.Errorf("%s: classify=%d want %d", tc.name, got, tc.want)
		}
	}
}

// TestControlMessageNeverEnqueues is the core regression for the group-
// announcement / GROUP.md-updated false-trigger bug: a Text(=1) payload with an
// `event` envelope, a non-empty from_uid, and a bot @-mention must NOT enqueue.
func TestControlMessageNeverEnqueues(t *testing.T) {
	c := NewConnector(NewRESTClient("http://x", func() string { return "tk" }))
	c.setUID("bot1")
	c.onInbound(BotMessage{
		MessageID:   "m1",
		FromUID:     "operator-uid",
		ChannelID:   "g1",
		ChannelType: ChannelGroup,
		Payload: MessagePayload{
			Type:    MsgText,
			Content: "GROUP.md updated",
			Mention: &Mention{UIDs: []string{"bot1"}}, // would be ReasonExplicitBot
			Event:   &PayloadEvent{Type: EventGroupMdUpdated, Version: 2},
		},
	})

	c.mu.Lock()
	n := len(c.turnQueues)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("control message enqueued a turn (turnQueues=%d), want 0", n)
	}
}

func TestRESTGetGroupMd(t *testing.T) {
	var mdPath, threadMdPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/threads/"):
			threadMdPath = r.URL.Path
			_ = json.NewEncoder(w).Encode(GroupMd{Content: "thread doc", Version: 5})
		default:
			mdPath = r.URL.Path
			_ = json.NewEncoder(w).Encode(GroupMd{Content: "group doc", Version: 7, UpdatedBy: "u_c"})
		}
	}))
	defer srv.Close()

	c := NewRESTClient(srv.URL, func() string { return "tk" })
	ctx := context.Background()

	md, err := c.GetGroupMd(ctx, "g1")
	if err != nil || md.Content != "group doc" || md.Version != 7 {
		t.Fatalf("GetGroupMd wrong: %+v err=%v", md, err)
	}
	if mdPath != "/v1/bot/groups/g1/md" {
		t.Fatalf("group md path = %q", mdPath)
	}

	tmd, err := c.GetThreadMd(ctx, "g1", "t9")
	if err != nil || tmd.Content != "thread doc" {
		t.Fatalf("GetThreadMd wrong: %+v err=%v", tmd, err)
	}
	if threadMdPath != "/v1/bot/groups/g1/threads/t9/md" {
		t.Fatalf("thread md path = %q", threadMdPath)
	}
}

// TestMirrorGroupDocWritesAndDeletes drives the mirror path: a non-empty server
// md is written to local GROUP.md; an empty server md removes it.
func TestMirrorGroupDocWritesAndDeletes(t *testing.T) {
	content := "# 群手册\n请先看 PR 模板。"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(GroupMd{Content: content, Version: 1})
	}))
	defer srv.Close()

	cwd := t.TempDir()
	c := NewConnector(NewRESTClient(srv.URL, func() string { return "tk" }))
	c.mirrorGroupDoc(context.Background(), "g1", cwd)

	got, err := safepath.SafeRead(cwd, groupDocFilename, groupDocMaxBytes)
	if err != nil {
		t.Fatalf("read GROUP.md: %v", err)
	}
	if string(got) != content {
		t.Errorf("GROUP.md mirror mismatch:\ngot:  %q\nwant: %q", got, content)
	}

	// Now the server md is cleared → mirror must remove the local file.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(GroupMd{Content: "", Version: 2})
	}))
	defer srv2.Close()
	c2 := NewConnector(NewRESTClient(srv2.URL, func() string { return "tk" }))
	c2.mirrorGroupDoc(context.Background(), "g1", cwd)
	if _, err := os.Stat(filepath.Join(cwd, groupDocFilename)); !os.IsNotExist(err) {
		t.Errorf("empty server md should remove local GROUP.md, stat err=%v", err)
	}
}

// TestMirrorGroupDocThreadUsesThreadEndpoint proves a thread channel id routes
// to the thread md endpoint, not the group one.
func TestMirrorGroupDocThreadUsesThreadEndpoint(t *testing.T) {
	var hitThread bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/threads/") {
			hitThread = true
		}
		_ = json.NewEncoder(w).Encode(GroupMd{Content: "x", Version: 1})
	}))
	defer srv.Close()

	cwd := t.TempDir()
	c := NewConnector(NewRESTClient(srv.URL, func() string { return "tk" }))
	c.mirrorGroupDoc(context.Background(), "g1"+ThreadIDSeparator+"t9", cwd)
	if !hitThread {
		t.Fatal("thread channel id should hit the thread md endpoint")
	}
}

func TestClaimSysRefreshDebounces(t *testing.T) {
	c := NewConnector(NewRESTClient("http://x", func() string { return "tk" }))
	if !c.claimSysRefresh("g1") {
		t.Fatal("first claim should succeed")
	}
	if c.claimSysRefresh("g1") {
		t.Fatal("second claim within window should be debounced")
	}
	if !c.claimSysRefresh("g2") {
		t.Fatal("different channel should not be debounced")
	}
}
