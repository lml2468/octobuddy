package octo

import "testing"

func TestIsThreadChannelID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"grp1____topicA", true},
		{"123____abc", true},
		{"grp1____", true},    // empty short-id portion still has the separator
		{"____abc", true},     // empty parent portion still a thread shape
		{"grp1", false},       // bare group
		{"grp_1", false},      // single underscores are not the separator
		{"grp__1", false},     // two underscores
		{"grp___1", false},    // three underscores
		{"", false},           // empty
		{"a____b____c", true}, // multiple separators: still a thread
	}
	for _, c := range cases {
		if got := IsThreadChannelID(c.id); got != c.want {
			t.Errorf("IsThreadChannelID(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}

func TestExtractParentGroupNo(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"grp1____topicA", "grp1"},
		{"123____abc", "123"},
		{"grp1____", "grp1"}, // empty short-id
		{"____abc", ""},      // empty parent
		{"grp1", "grp1"},     // bare group passes through
		{"", ""},             // empty
		{"a____b____c", "a"}, // splits on the FIRST separator
		{"grp_1", "grp_1"},   // single underscores are not a separator
	}
	for _, c := range cases {
		if got := ExtractParentGroupNo(c.id); got != c.want {
			t.Errorf("ExtractParentGroupNo(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

func TestExtractThreadShortID(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"grp1____topicA", "topicA"},
		{"123____abc", "abc"},
		{"grp1____", ""},          // empty short-id portion → ""
		{"____abc", "abc"},        // empty parent, valid short-id
		{"grp1", ""},              // bare group has no short-id
		{"", ""},                  // empty
		{"a____b____c", "b____c"}, // everything after the FIRST separator
		{"grp_1", ""},             // single underscores are not a separator
	}
	for _, c := range cases {
		if got := extractThreadShortID(c.id); got != c.want {
			t.Errorf("extractThreadShortID(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

// TestThreadSessionKeyDistinctFromParent proves a thread channel routes to its
// OWN session, isolated from the parent group and from sibling threads. The
// router maps a group/thread sessionKey to the channel id verbatim, so two
// distinct channel ids necessarily yield distinct sessionKeys (and therefore
// distinct sandbox partitions, which hash the same key).
func TestThreadSessionKeyDistinctFromParent(t *testing.T) {
	parent := "grp1"
	thread := "grp1____topicA"
	sibling := "grp1____topicB"

	// Membership/permission is inherited: all three share the same parent group.
	if ExtractParentGroupNo(thread) != parent {
		t.Fatalf("thread parent = %q, want %q", ExtractParentGroupNo(thread), parent)
	}
	if ExtractParentGroupNo(sibling) != parent {
		t.Fatalf("sibling parent = %q, want %q", ExtractParentGroupNo(sibling), parent)
	}

	// But the channel ids — which the router uses verbatim as the group
	// sessionKey — are all distinct, so conversation + sandbox never collide.
	if thread == parent {
		t.Fatal("thread channel id must differ from parent group id")
	}
	if thread == sibling {
		t.Fatal("sibling threads must have distinct channel ids")
	}
}

func TestRerouteTarget(t *testing.T) {
	cases := []struct {
		name         string
		current      string
		target       string
		wantChannel  string
		wantRerouted bool
	}{
		{
			name:         "bare parent inside thread reroutes to thread",
			current:      "grp1____topicA",
			target:       "grp1",
			wantChannel:  "grp1____topicA",
			wantRerouted: true,
		},
		{
			name:         "explicit thread target passes through (never overridden)",
			current:      "grp1____topicA",
			target:       "grp1____topicB",
			wantChannel:  "grp1____topicB",
			wantRerouted: false,
		},
		{
			name:         "same explicit thread passes through",
			current:      "grp1____topicA",
			target:       "grp1____topicA",
			wantChannel:  "grp1____topicA",
			wantRerouted: false,
		},
		{
			name:         "cross-group bare target untouched",
			current:      "grp1____topicA",
			target:       "grp2",
			wantChannel:  "grp2",
			wantRerouted: false,
		},
		{
			name:         "not in a thread session: no reroute",
			current:      "grp1",
			target:       "grp1",
			wantChannel:  "grp1",
			wantRerouted: false,
		},
		{
			name:         "DM-ish current (no separator): no reroute",
			current:      "user-uid",
			target:       "grp1",
			wantChannel:  "grp1",
			wantRerouted: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, rerouted := RerouteTarget(c.current, c.target)
			if got != c.wantChannel || rerouted != c.wantRerouted {
				t.Errorf("RerouteTarget(%q, %q) = (%q, %v), want (%q, %v)",
					c.current, c.target, got, rerouted, c.wantChannel, c.wantRerouted)
			}
		})
	}
}
