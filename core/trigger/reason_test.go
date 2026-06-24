package trigger

import "testing"

// TestReasonIsAmbiguousAddressing pins the closed enum behind the bot-loop
// guard's scoping decision. Router/gate consults this method instead of
// pattern-matching the enum value; flipping a reason between ambiguous
// and unambiguous (or adding a new one) must be a one-line change here
// and gate.go remains untouched.
//
// Why this test exists: the bot-loop guard regressed once during the
// #105 refactor (a wider gate dropped legitimate peer-bot @-grantor
// mentions) and the fix bolted on a hard-coded `Reason ==
// ReasonMentionFreeGroup` check. #118 lifted that decision back to the
// trigger pkg — this test is the gate.
func TestReasonIsAmbiguousAddressing(t *testing.T) {
	// Ambiguous: classifier flagged this as a reply candidate but message
	// metadata can't tell us if the sender meant to address the bot.
	ambiguous := map[Reason]bool{ReasonMentionFreeGroup: true}
	for r := range ambiguous {
		if !r.IsAmbiguousAddressing() {
			t.Errorf("%q must be ambiguous-addressing (loop guard applies)", r)
		}
	}
	// Unambiguous: addressing intent is clear, peer-bot senders must
	// pass through. Includes ReasonNone (no classification → no scope
	// for the loop guard) and the post-classifier non-reply reasons
	// (observation / obo_irrelevant) which the router never sees but
	// must stay safely on the unambiguous side of any future caller.
	unambiguous := map[Reason]bool{
		ReasonNone:           true,
		ReasonDM:             true,
		ReasonExplicitBot:    true,
		ReasonPersonaGrantor: true,
		ReasonPersonaHumans:  true,
		ReasonReplyToBot:     true,
		ReasonAIBroadcast:    true,
		ReasonCron:           true,
		ReasonObservation:    true,
		ReasonOBOIrrelevant:  true,
	}
	for r := range unambiguous {
		if r.IsAmbiguousAddressing() {
			t.Errorf("%q must NOT be ambiguous-addressing", r)
		}
	}

	// Coverage closure: every Reason in AllReasons must appear in
	// exactly one of the two partitions. A new constant added to types.go
	// but forgotten here (or vice versa) fails loudly instead of
	// silently defaulting through IsAmbiguousAddressing's `default:
	// return false` branch. This catches the failure mode the test
	// previously couldn't: enum drift.
	for _, r := range AllReasons {
		inA := ambiguous[r]
		inU := unambiguous[r]
		if inA == inU {
			t.Errorf("Reason %q must be in exactly one partition (ambiguous=%v unambiguous=%v); add it to one and to IsAmbiguousAddressing's switch", r, inA, inU)
		}
	}
	if len(ambiguous)+len(unambiguous) != len(AllReasons) {
		t.Errorf("partition coverage drift: ambiguous=%d unambiguous=%d AllReasons=%d", len(ambiguous), len(unambiguous), len(AllReasons))
	}
}
