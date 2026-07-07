package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeBin writes an executable shell script and returns its path.
func writeFakeBin(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fakebin.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestMergedEnvAllowlistsAndOverrides exercises the env-allowlist contract:
// - non-allowlisted operator vars are DROPPED before the agent sees them
// (hardening: a prompt-injected agent shouldn't inherit
// AWS_*/GH_TOKEN/OPENAI_API_KEY/SSH_AUTH_SOCK from the operator's shell).
// - allowlisted vars pass through.
// - LC_* family auto-passes (locale).
// - `extra` is appended last so overrides win.
func TestMergedEnvAllowlistsAndOverrides(t *testing.T) {
	// Set: one allowlisted (LANG), one LC_* family (LC_TIME), one explicitly
	// non-allowlisted (AWS_SECRET_ACCESS_KEY — the canonical example of what
	// must NOT leak into the agent).
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("LC_TIME", "en_US.UTF-8")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "AKIA-leak-canary")

	env := mergedEnv([]string{"OCTOBUDDY_TEST_EXTRA=added", "LANG=override-via-extra"})
	seen := scanMergedEnv(env)
	if !seen.sawLang {
		t.Error("LANG (allowlisted) did not pass through")
	}
	if !seen.sawLCTime {
		t.Error("LC_TIME (LC_* family) did not pass through")
	}
	if seen.sawAWS {
		t.Error("AWS_SECRET_ACCESS_KEY (non-allowlisted) leaked through — env allowlist regression")
	}
	if !seen.sawExtra {
		t.Error("extra entry did not append")
	}
	if seen.lastLang != "override-via-extra" {
		t.Errorf("extra must come last (win), last LANG = %q", seen.lastLang)
	}
}

type mergedEnvSeen struct {
	sawLang   bool
	sawLCTime bool
	sawAWS    bool
	sawExtra  bool
	lastLang  string
}

func scanMergedEnv(env []string) mergedEnvSeen {
	var seen mergedEnvSeen
	for _, e := range env {
		switch {
		case e == "LANG=en_US.UTF-8":
			seen.sawLang = true
			seen.lastLang = "from-os"
		case e == "LANG=override-via-extra":
			seen.lastLang = "override-via-extra"
		case e == "LC_TIME=en_US.UTF-8":
			seen.sawLCTime = true
		case e == "AWS_SECRET_ACCESS_KEY=AKIA-leak-canary":
			seen.sawAWS = true
		case e == "OCTOBUDDY_TEST_EXTRA=added":
			seen.sawExtra = true
		}
	}
	return seen
}

// TestClaudeDriverInjectsEnv spawns a fake "claude" that echoes an env var; the
// driver should have set it. The echoed line is not stream-json, so it surfaces
// as a KindSystem event — we just assert the value made it into the subprocess.
func TestClaudeDriverInjectsEnv(t *testing.T) {
	bin := writeFakeBin(t, `echo "GOT:$OCTOBUDDY_INJECTED"`)
	d := NewClaudeDriver(bin)
	d.StaticEnv = []string{"OCTOBUDDY_INJECTED=hello-env"}

	ch, err := d.Query(context.Background(), Request{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for ev := range ch {
		if ev.Kind == KindSystem && ev.Text == "GOT:hello-env" {
			found = true
		}
		if ev.Raw == "GOT:hello-env" {
			found = true
		}
	}
	if !found {
		t.Fatal("injected env var did not reach the spawned CLI")
	}
}

// TestClaudeDriverDrainsStdoutAndStderr spawns a fake "claude" that writes to
// both stdout (stream-json) and stderr, then exits non-zero. The driver must
// deliver events from both streams and close the channel cleanly — exercising
// the WaitGroup join that prevents a send-on-closed-channel panic when stderr
// emits around the time stdout reaches EOF.
func TestClaudeDriverDrainsStdoutAndStderr(t *testing.T) {
	bin := writeFakeBin(t, `
echo '{"type":"system","subtype":"init","session_id":"s1"}'
echo 'a warning on stderr' 1>&2
echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}'
exit 3
`)
	d := NewClaudeDriver(bin)

	ch, err := d.Query(context.Background(), Request{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}

	events := collectClaudeDrainEvents(ch)
	if !events.session || !events.text {
		t.Fatalf("missing stdout events: session=%v text=%v", events.session, events.text)
	}
	if !events.stderrErr {
		t.Fatal("stderr line was not surfaced as a recoverable error event")
	}
	if !events.exitErr {
		t.Fatal("non-zero exit was not surfaced as an error event")
	}
}

type claudeDrainEvents struct {
	session   bool
	text      bool
	stderrErr bool
	exitErr   bool
}

func collectClaudeDrainEvents(ch <-chan AgentEvent) claudeDrainEvents {
	var events claudeDrainEvents
	for ev := range ch {
		markClaudeDrainEvent(&events, ev)
	}
	return events
}

func markClaudeDrainEvent(events *claudeDrainEvents, ev AgentEvent) {
	switch {
	case ev.Kind == KindSessionStarted && ev.SessionID == "s1":
		events.session = true
	case ev.Kind == KindTextDelta && ev.Text == "hi":
		events.text = true
	case ev.Kind == KindError && ev.Recoverable && ev.Err == "a warning on stderr":
		events.stderrErr = true
	case ev.Kind == KindError && strings.Contains(ev.Err, "claude exited"):
		events.exitErr = true
	}
}

func TestCompleteTextEmitted(t *testing.T) {
	// Plain stream-json (no --include-partial-messages): the complete assistant
	// text block is the reply and must be emitted.
	bin := writeFakeBin(t, `
echo '{"type":"system","subtype":"init","session_id":"s1"}'
echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"plain"}]}}'
`)
	ch, err := NewClaudeDriver(bin).Query(context.Background(), Request{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for ev := range ch {
		if ev.Kind == KindTextDelta {
			text += ev.Text
		}
	}
	if text != "plain" {
		t.Fatalf("complete text = %q, want %q", text, "plain")
	}
}

// TestPromptFedOnStdin asserts the driver passes the prompt via stdin (`-p -`),
// not as an argv element. The fake agent echoes its fd-0 contents on stderr,
// which the driver surfaces as a recoverable error event.
func TestPromptFedOnStdin(t *testing.T) {
	bin := writeFakeBin(t, `
data=$(cat)
echo '{"type":"system","subtype":"init","session_id":"s1"}'
printf 'STDIN:[%s]\n' "$data" 1>&2
`)
	ch, err := NewClaudeDriver(bin).Query(context.Background(), Request{Prompt: "hello-prompt"})
	if err != nil {
		t.Fatal(err)
	}
	var report string
	for ev := range ch {
		if strings.HasPrefix(ev.Err, "STDIN:") {
			report = ev.Err
		}
	}
	if report != "STDIN:[hello-prompt]" {
		t.Fatalf("prompt not fed on stdin; agent saw %q", report)
	}
}

// TestAgentStdinIsPromptNotTokenPipe locks the control-bus capability-token
// boundary at the agent hop, hardening follow-up from the review
// of PR #63). The daemon receives its cap token on fd 0 — a private pipe — but
// the agent CLI it spawns must NEVER inherit that fd. The driver now feeds the
// PROMPT on the child's fd 0 (`-p -`) via a private strings.Reader, so the
// guarantee is: the child sees exactly the prompt and nothing of this process's
// os.Stdin. The concrete regression this catches is a change that set
// cmd.Stdin = os.Stdin (or left it inheriting): the sentinel token below flows
// through os.Stdin, so a leak becomes observable.
//
// We stand in for the daemon by feeding a sentinel token onto this process's
// os.Stdin, then spawn the driver with a distinct prompt and assert the fake
// agent reads back the PROMPT (not the token) from its fd 0.
func TestAgentStdinIsPromptNotTokenPipe(t *testing.T) {
	const token = "OCTOBUDDY-CAP-TOKEN-sentinel-do-not-leak-7f3a9c"
	const prompt = "the-real-prompt-payload"

	r, w := seedTokenPipe(t, token)

	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = oldStdin
		w.Close()
		r.Close()
	})

	report, sawReport := captureFakeAgentStdinReport(t, prompt)
	if !sawReport {
		t.Fatal("fake agent did not report its fd 0 contents")
	}
	if strings.Contains(report, token) {
		t.Fatalf("agent fd 0 leaked the daemon capability token: %q", report)
	}
	if report != "STDIN0:["+prompt+"]" {
		t.Fatalf("agent fd 0 must carry exactly the prompt; got %q", report)
	}
}

func seedTokenPipe(t *testing.T, token string) (*os.File, *os.File) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 256; i++ {
		if _, err := w.WriteString(token + "\n"); err != nil {
			t.Fatalf("seed token pipe: %v", err)
		}
	}
	return r, w
}

func captureFakeAgentStdinReport(t *testing.T, prompt string) (string, bool) {
	t.Helper()

	bin := writeFakeBin(t, `
data=$(cat)
echo '{"type":"system","subtype":"init","session_id":"s1"}'
printf 'STDIN0:[%s]\n' "$data" 1>&2
`)

	ch, err := NewClaudeDriver(bin).Query(context.Background(), Request{Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}

	var report string
	var sawReport bool
	for ev := range ch {
		if strings.HasPrefix(ev.Err, "STDIN0:") && !sawReport {
			report, sawReport = ev.Err, true
		}
	}
	return report, sawReport
}
