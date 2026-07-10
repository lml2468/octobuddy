package agent

import "testing"

// TestTagPoisonedResume pins the sessionID-guarded tagging that the driver Query
// wrappers apply. A terminal, non-transient, poison-shaped error is marked
// Poisoned ONLY when the turn carried a resume id — the exact guard that keeps a
// genuine fresh-turn 400 (no poisonable history) from being swallowed.
func TestTagPoisonedResume(t *testing.T) {
	poison := AgentEvent{Kind: KindError, Err: "invalid_request_error: messages too long", Recoverable: false}
	transient := AgentEvent{Kind: KindError, Err: "429 rate limit reached", Recoverable: false, Transient: true}
	recoverable := AgentEvent{Kind: KindError, Err: "invalid_request_error (mid-turn)", Recoverable: true}
	resumeBad := AgentEvent{Kind: KindError, Err: "No conversation found with session ID", Recoverable: true, ResumeInvalid: true}

	// Resumed turn: only the terminal, non-transient poison event is tagged.
	got := tagPoisonedResume([]AgentEvent{poison, transient, recoverable, resumeBad}, "resume-id")
	if !got[0].Poisoned {
		t.Errorf("terminal poison on a resumed turn must be tagged Poisoned")
	}
	if got[1].Poisoned {
		t.Errorf("transient error must never be Poisoned (transient wins)")
	}
	if got[2].Poisoned {
		t.Errorf("recoverable (non-terminal) error must not be Poisoned")
	}
	if got[3].Poisoned {
		t.Errorf("ResumeInvalid error must not be re-tagged Poisoned")
	}

	// Fresh turn (no resume id): the SAME poison error is NOT tagged — the
	// regression guard for the empty-reply defect.
	fresh := tagPoisonedResume([]AgentEvent{poison}, "")
	if fresh[0].Poisoned {
		t.Errorf("fresh-turn terminal error must NOT be Poisoned (no resumable history)")
	}
}

// TestIsPoisonedResume pins the classifier that distinguishes a
// deterministically-reproducing resume failure (baked into the conversation
// history) from a transient upstream blip. Transient always wins.
func TestIsPoisonedResume(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"invalid_request_error", `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`, true},
		{"plain invalid request", "API Error: invalid request: messages too long", true},
		{"maximum turns reached", "Agent stopped: maximum turns reached", true},
		// Claude's real structured result shape uses an underscore subtype with no
		// trailing "reached" — the regression the reviewer caught.
		{"error_max_turns subtype", "result error (subtype=error_max_turns): hit max turns", true},
		{"hit max turns prose", "hit max turns", true},
		{"iteration limit", "iteration limit exceeded", true},
		{"gave up", "the model gave up after repeated failures", true},
		{"reached maximum number", "reached the maximum number of tokens", true},
		// Transient wins: a 429/503 is retryable, never poison.
		{"rate limit 429 not poison", "HTTP 429 rate limit reached, resets at 3pm", false},
		{"503 not poison", "503 service unavailable", false},
		{"overloaded not poison", "overloaded_error: server overloaded", false},
		// Ordinary content is not poison.
		{"benign", "hello world, here is your summary", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPoisonedResume(tc.in); got != tc.want {
				t.Errorf("isPoisonedResume(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
