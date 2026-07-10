package main

import (
	"testing"

	"github.com/lml2468/octobuddy/core/gateway"
	"github.com/lml2468/octobuddy/core/im/octo"
	"github.com/lml2468/octobuddy/core/trigger"
)

// TestGroupDocInjectCapCoversMirrorCap locks the cross-package invariant that
// was previously comment-only (P4 leak absorption): the gateway injects GROUP.md
// via safepath.SafeRead, which ERRORS (not truncates) past its cap, while the
// octo connector mirrors GROUP.md truncated to its own cap. If the gateway cap
// were smaller, a large-but-valid mirrored GROUP.md would silently vanish from
// the prompt. The daemon is the one place that imports both packages, so the
// assertion lives here.
func TestGroupDocInjectCapCoversMirrorCap(t *testing.T) {
	if gateway.GroupDocMaxInjectBytes < octo.GroupDocMaxBytes {
		t.Fatalf("gateway inject cap %d must be >= octo mirror cap %d — a valid mirrored GROUP.md would be dropped",
			gateway.GroupDocMaxInjectBytes, octo.GroupDocMaxBytes)
	}
}

// TestOBODMKindMatchesOctoChannelDM locks the OBO wire convention: trigger's
// IM-agnostic DM-kind code must equal the octo connector's ChannelDM value, so
// an OBO signal from octo is correctly recognized as a DM without trigger
// importing the IM package.
func TestOBODMKindMatchesOctoChannelDM(t *testing.T) {
	if trigger.DMKindWire != int(octo.ChannelDM) {
		t.Fatalf("trigger.DMKindWire (%d) must equal octo.ChannelDM (%d)",
			trigger.DMKindWire, int(octo.ChannelDM))
	}
}
