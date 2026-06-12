package groupctx

import (
	"strings"
	"testing"
)

func TestMembersReturnsLearnedSorted(t *testing.T) {
	gc := New(6000)
	gc.Push("c1", "u2", "bob", "hi")
	gc.Push("c1", "u1", "alice", "hey")
	gc.LearnMember("c1", "u3", "carol")
	// other channel is isolated
	gc.Push("c2", "u9", "zoe", "yo")

	got := gc.Members("c1")
	if len(got) != 3 {
		t.Fatalf("want 3 members, got %d: %+v", len(got), got)
	}
	// deterministic name-sorted order
	wantNames := []string{"alice", "bob", "carol"}
	for i, m := range got {
		if m.Name != wantNames[i] {
			t.Fatalf("member[%d].Name = %q, want %q (%+v)", i, m.Name, wantNames[i], got)
		}
	}
	if got[0].UID != "u1" {
		t.Fatalf("alice uid = %q, want u1", got[0].UID)
	}
	// unknown channel returns nil
	if m := gc.Members("nope"); m != nil {
		t.Fatalf("unknown channel should return nil, got %+v", m)
	}
}

func TestMemberListPrefixEmpty(t *testing.T) {
	gc := New(6000)
	if p := gc.MemberListPrefix("c1"); p != "" {
		t.Fatalf("no members should yield empty prefix, got %q", p)
	}
}

func TestMemberListPrefixInlineSmall(t *testing.T) {
	gc := New(6000)
	gc.Push("c1", "u1", "alice", "hi")
	gc.Push("c1", "u2", "bob", "yo")

	p := gc.MemberListPrefix("c1")
	if !strings.Contains(p, "[Group Members]") {
		t.Fatalf("missing inline header:\n%s", p)
	}
	if !strings.Contains(p, "  alice (u1)") || !strings.Contains(p, "  bob (u2)") {
		t.Fatalf("missing inline member line:\n%s", p)
	}
	if !strings.Contains(p, "ONE colon") {
		t.Fatalf("missing mention-format hint:\n%s", p)
	}
	// real-member example anchor
	if !strings.Contains(p, "@[u1:alice]") {
		t.Fatalf("missing real-member example anchor:\n%s", p)
	}
	if strings.Contains(p, "too many to list") {
		t.Fatalf("small roster should not emit look-up hint:\n%s", p)
	}
}

func TestMemberListPrefixLookupHintWhenLarge(t *testing.T) {
	gc := New(6000)
	for i := 0; i < 11; i++ {
		uid := string(rune('a'+i)) + "id"
		name := "member" + string(rune('A'+i))
		gc.Push("c1", uid, name, "hi")
	}
	p := gc.MemberListPrefix("c1")
	if !strings.Contains(p, "too many to list") {
		t.Fatalf(">10 members should emit look-up hint:\n%s", p)
	}
	if !strings.Contains(p, "11 members") {
		t.Fatalf("look-up hint should report count:\n%s", p)
	}
	if !strings.Contains(p, "ONE colon") {
		t.Fatalf("look-up hint should still carry mention-format hint:\n%s", p)
	}
	if strings.Contains(p, "[Group Members]") {
		t.Fatalf("large roster should not inline members:\n%s", p)
	}
}
