package agent

import "testing"

// TestSystemPromptFlattenOrder pins the Mandatory → Persona → Background order
// and that empty segments are skipped (no leading/trailing/double blank lines).
func TestSystemPromptFlattenOrder(t *testing.T) {
	sp := SystemPrompt{
		Mandatory:  "SEC",
		Persona:    []string{"SOUL", "", "ROSTER"},
		Background: []string{"HANDBOOK"},
	}
	got := sp.Flatten()
	want := "SEC\n\nSOUL\n\nROSTER\n\nHANDBOOK"
	if got != want {
		t.Fatalf("Flatten order/skip wrong:\n got %q\nwant %q", got, want)
	}
}

// TestSystemPromptFlattenEmpty pins that a zero value flattens to "".
func TestSystemPromptFlattenEmpty(t *testing.T) {
	if got := (SystemPrompt{}).Flatten(); got != "" {
		t.Fatalf("empty Flatten = %q, want \"\"", got)
	}
	if got := (SystemPrompt{Persona: []string{"", ""}}).Flatten(); got != "" {
		t.Fatalf("all-empty Persona Flatten = %q, want \"\"", got)
	}
}

// TestSystemPromptIsZero pins IsZero across the segment combinations.
func TestSystemPromptIsZero(t *testing.T) {
	cases := []struct {
		name string
		sp   SystemPrompt
		zero bool
	}{
		{"empty", SystemPrompt{}, true},
		{"empty persona slice", SystemPrompt{Persona: []string{"", ""}}, true},
		{"mandatory only", SystemPrompt{Mandatory: "X"}, false},
		{"persona only", SystemPrompt{Persona: []string{"X"}}, false},
		{"background only", SystemPrompt{Background: []string{"X"}}, false},
	}
	for _, tc := range cases {
		if got := tc.sp.IsZero(); got != tc.zero {
			t.Errorf("%s: IsZero = %v, want %v", tc.name, got, tc.zero)
		}
	}
}

// TestDriverConsumesSystemStruct pins that ClaudeDriver builds the system-prompt
// flag from Request.System (Flatten order preserved), the structured replacement
// for the removed flat SystemPrompt field.
func TestDriverConsumesSystemStruct(t *testing.T) {
	d := newTestDriver()
	args := d.buildArgs(Request{Prompt: "hi", System: SystemPrompt{
		Mandatory: "SEC",
		Persona:   []string{"SOUL"},
	}})
	got, ok := systemPromptArg(args)
	if !ok {
		t.Fatal("expected --system-prompt in minimal mode")
	}
	if got != "SEC\n\nSOUL" {
		t.Fatalf("driver should flatten System: got %q", got)
	}
}
