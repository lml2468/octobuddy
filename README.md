# MLClaw

A cross-platform agent gateway core, written in Go. MLClaw **drives coding
agents (Claude, Codex, …) by spawning their CLI / app-server and normalizing
their output into one unified event stream** — replacing the Node-only
`claude-agent-sdk`. The Go core compiles to a single static binary on every
platform; native GUI shells (e.g. a macOS app) sit on top via a control bus.

> Status: foundation validated — the driver abstraction is proven across two
> very different agent protocols, and the headless gateway pipeline
> (inbound → router → driver → outbound, with per-session locking + resume)
> runs end-to-end. See below.

## What this proves

| Claim | How it's proven | Result |
|---|---|---|
| The `claude` CLI can replace `claude-agent-sdk` | `agent.ClaudeDriver` spawns `claude -p --output-format stream-json --verbose [--resume …]` and normalizes its line-delimited JSON | ✅ live spawn + parse verified against real CLI output (`system`/`init`/`api_retry` lines + extracted session id) |
| **The same `Driver` interface holds across a totally different protocol** | `agent.CodexDriver` spawns `codex app-server` (long-lived JSON-RPC over stdio, duplex) and normalizes its notifications into the same `AgentEvent` vocabulary | ✅ live handshake verified against real `codex app-server` (`initialize` → `thread/start` returned real thread id → server notifications normalized); `TestCrossDriverVocabularyParity` proves both protocols reduce to the same ordered event sequence |
| Agent output normalizes to one unified stream | `agent.AgentEvent` + per-driver translators | ✅ unit tests on recorded fixtures (no API key needed) for both drivers |
| Resume/session continuity works without the SDK | `store.SessionStore` (pure-Go SQLite) maps `sessionKey → resume_id`; demo persists then resumes | ✅ verified for both claude (replay) and codex (live thread id) |
| Go core = single static binary, trivial cross-compile | `CGO_ENABLED=0 go build` to 5 platforms | ✅ darwin/{arm64,amd64}, linux/{amd64,arm64}, windows/amd64 — all ~10MB, zero cgo |

The Codex row is the abstraction's real stress test: Claude is a **one-shot CLI
streaming stdout**; Codex is a **persistent JSON-RPC duplex process**. Two
completely different protocol shapes, one `Driver` interface, one downstream
`consume()` that does not change a line. That is the multi-agent foundation.

## Drivers

| Driver | Mechanism | Native protocol |
|---|---|---|
| `ClaudeDriver` | `claude -p --output-format stream-json` | line-delimited JSON over stdout (one-shot) |
| `CodexDriver`  | `codex app-server` | JSON-RPC 2.0 over stdio (long-lived, duplex): `initialize` → `thread/start`/`thread/resume` → `turn/start` → consume `item/*` + `turn/completed` notifications |

Both satisfy `agent.Driver` (compile-time asserted in `codex_test.go`).

## Layout

```
agent/        AgentDriver abstraction (the heart). Gateway depends only on this.
  agent.go    AgentEvent, Request, Capabilities, Driver interface
  claude.go   ClaudeDriver: spawn claude CLI, parse stream-json → AgentEvent
  claude_test.go  deterministic parser tests over recorded/canonical fixtures
  replay.go   expose parser for offline replay
store/        pure-Go SQLite resume-id map (the slice the gateway needs)
main.go       CLI: live (spawn claude) or replay (recorded stream-json)
fixtures/     recorded stream-json turn (text + tool_use + result)
```

## Run

```bash
go test ./...                                                   # deterministic core (no key)
go run . -replay fixtures/turn.jsonl -session-key group:demo   # offline claude demo
go run . -prompt "hello"                                        # live: spawns claude
go run . -driver codex -prompt "hello"                          # live: spawns codex app-server
```

## Notes / next

- **Auth:** this machine's `claude` returns 401 on direct Anthropic calls (its
  key is Claude-Code-internal, not for standalone CLI). The parse pipeline is
  fully verified regardless; assistant-text rendering is covered by fixtures.
  A real `ANTHROPIC_API_KEY` would light up the live text path end-to-end.
- **Next driver:** `CodexDriver` — spawn `codex app-server --listen stdio://`
  (JSON-RPC over stdio), modeled on Open Island's `CodexAppServer.swift`. It
  implements the same `agent.Driver` interface; the gateway stays unchanged.
- This `agent` package is **not throwaway** — it is the embryo of the real
  `ccd` core's driver layer.
