// Command ccd-spike validates that the Go AgentDriver abstraction can drive
// Claude Code via its CLI (replacing claude-agent-sdk) and normalize its
// stream-json into a unified AgentEvent stream.
//
// Two modes:
//
//	ccd-spike -prompt "hello"            # live: spawns `claude -p ...`
//	ccd-spike -replay fixtures/turn.jsonl # offline: replays recorded stream-json
//
// In both modes it prints the normalized events and persists the session id to
// a pure-Go SQLite store (the resume map the real gateway needs).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mlamp/mlclaw/agent"
	"github.com/mlamp/mlclaw/store"
)

func main() {
	var (
		prompt     = flag.String("prompt", "", "prompt to send to the agent (live mode)")
		replay     = flag.String("replay", "", "path to a recorded stream-json file (offline mode, claude only)")
		driverName = flag.String("driver", "claude", "agent driver: claude | codex")
		bin        = flag.String("bin", "", "agent executable (default: driver name)")
		sessionKey = flag.String("session-key", "spike:default", "logical session key for resume persistence")
		dbPath     = flag.String("db", filepath.Join(os.TempDir(), "ccd-spike.db"), "sqlite path")
		model      = flag.String("model", "", "optional model override")
	)
	flag.Parse()

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "store open: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	resume, _ := st.Resume(*sessionKey)
	if resume != "" {
		fmt.Printf("↻ resuming session %s (key=%s)\n", resume, *sessionKey)
	} else {
		fmt.Printf("✦ new session (key=%s)\n", *sessionKey)
	}

	var events <-chan agent.AgentEvent
	if *replay != "" {
		events = replayFile(*replay)
	} else {
		if *prompt == "" {
			fmt.Fprintln(os.Stderr, "provide -prompt (live) or -replay <file> (offline)")
			os.Exit(2)
		}
		var d agent.Driver
		switch *driverName {
		case "claude":
			d = agent.NewClaudeDriver(*bin)
		case "codex":
			d = agent.NewCodexDriver(*bin)
		default:
			fmt.Fprintf(os.Stderr, "unknown driver %q (claude|codex)\n", *driverName)
			os.Exit(2)
		}
		fmt.Printf("driver=%s caps=%+v\n", d.Name(), d.Capabilities())
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		ch, err := d.Query(ctx, agent.Request{
			Prompt:    *prompt,
			SessionID: resume,
			Model:     *model,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "query: %v\n", err)
			os.Exit(1)
		}
		events = ch
	}

	consume(events, st, *sessionKey)
}

// consume renders the normalized event stream and persists the session id —
// exactly what the real gateway's stream-relay + session-store would do.
func consume(events <-chan agent.AgentEvent, st *store.SessionStore, sessionKey string) {
	var (
		fullText strings.Builder
		toolN    int
		newSess  string
	)
	for ev := range events {
		switch ev.Kind {
		case agent.KindSessionStarted:
			newSess = ev.SessionID
			fmt.Printf("  [session] %s\n", ev.SessionID)
		case agent.KindTextDelta:
			fullText.WriteString(ev.Text)
			fmt.Printf("  [text]    %s\n", oneLine(ev.Text))
		case agent.KindThinking:
			fmt.Printf("  [think]   %s\n", oneLine(ev.Text))
		case agent.KindToolUse:
			toolN++
			fmt.Printf("  [tool]    🔧 %s(%s)\n", ev.ToolName, ev.ToolParams)
		case agent.KindToolResult:
			fmt.Printf("  [result]  (tool returned)\n")
		case agent.KindTurnDone:
			if ev.Usage != nil {
				fmt.Printf("  [done]    in=%d out=%d tokens\n", ev.Usage.InputTokens, ev.Usage.OutputTokens)
			} else {
				fmt.Printf("  [done]\n")
			}
		case agent.KindError:
			tag := "ERR"
			if ev.Recoverable {
				tag = "retry"
			}
			fmt.Printf("  [%s]   %s\n", tag, oneLine(ev.Err))
		case agent.KindSystem:
			fmt.Printf("  [sys]     %s\n", oneLine(ev.Text))
		}
	}

	if newSess != "" {
		if err := st.SaveResume(sessionKey, "claude", newSess, time.Now().Unix()); err != nil {
			fmt.Fprintf(os.Stderr, "save resume: %v\n", err)
		} else {
			fmt.Printf("✓ persisted resume id %s for key %s\n", newSess, sessionKey)
		}
	}

	fmt.Printf("\n── summary ──\n")
	fmt.Printf("assistant text: %q\n", oneLine(fullText.String()))
	fmt.Printf("tool calls:     %d\n", toolN)
}

func replayFile(path string) <-chan agent.AgentEvent {
	out := make(chan agent.AgentEvent, 64)
	go func() {
		defer close(out)
		f, err := os.Open(path)
		if err != nil {
			out <- agent.AgentEvent{Kind: agent.KindError, Err: err.Error()}
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			for _, ev := range agent.ParseLineForReplay(line) {
				out <- ev
			}
		}
	}()
	return out
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 100 {
		s = s[:100] + "…"
	}
	return s
}
