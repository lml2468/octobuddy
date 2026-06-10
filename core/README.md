# XClaw core (`xclawd`)

The XClaw gateway daemon, written in Go. It **drives coding agents (Claude,
Codex, …) by spawning their CLI / app-server and normalizing their output into
one unified event stream** — replacing the Node-only `claude-agent-sdk`. Single
static binary, zero cgo, cross-compiles everywhere. Native shells (the Swift
macOS app in `../app`) sit on top via the control bus (`../proto`).

> Status: headless MVP runs end-to-end — `inbound → router (lock + gate +
> rate limit) → gateway → agent driver → outbound sink`, with SQLite-backed
> sessions/messages and per-session resume continuity. Driver abstraction
> proven across two very different agent protocols.

## What this proves

| Claim | How it's proven | Result |
|---|---|---|
| The `claude` CLI can replace `claude-agent-sdk` | `agent.ClaudeDriver` spawns `claude -p --output-format stream-json --verbose [--resume …]` and normalizes its line-delimited JSON | ✅ live spawn + parse verified against real CLI output (`system`/`init`/`api_retry` lines + extracted session id) |
| **The same `Driver` interface holds across a totally different protocol** | `agent.CodexDriver` spawns `codex app-server` (long-lived JSON-RPC over stdio, duplex) and normalizes its notifications into the same `AgentEvent` vocabulary | ✅ live handshake verified against real `codex app-server` (`initialize` → `thread/start` returned real thread id → server notifications normalized); `TestCrossDriverVocabularyParity` proves both protocols reduce to the same ordered event sequence |
| Agent output normalizes to one unified stream | `agent.AgentEvent` + per-driver translators | ✅ unit tests on recorded fixtures (no API key needed) for both drivers |
| Resume/session continuity works without the SDK | `store.Store` (pure-Go SQLite) maps `sessionKey → resume_id`; gateway persists then resumes | ✅ verified by `gateway` tests (second turn carries the first turn's resume id) and live codex thread ids |
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
cmd/xclawd/   daemon entry point — wires store+router+gateway+driver; ships a
              REPL inbound source (stdin) for the headless MVP.
agent/        Driver abstraction (the heart). Everything above depends only on this.
  agent.go    AgentEvent, Request, Capabilities, Driver interface
  claude.go   ClaudeDriver: spawn claude CLI, parse stream-json → AgentEvent
  codex.go    CodexDriver: spawn codex app-server, JSON-RPC → AgentEvent
  replay.go   expose parser for offline replay
store/        SQLite persistence: sessions + messages + resume map, 7-day TTL
router/       sessionKey derivation + per-session serial lock + 3-bucket rate limit
gateway/      handleMessage orchestration: route → store → driver → sink → persist
control/      control bus: NDJSON-over-UDS server + gateway EventSink (GUI clients)
im/octo/      Octo IM connector: WuKongIM binary protocol (curve25519 DH + MD5→
              AES-128-CBC) + REST; inbound → router, replies via REST. Ported
              wire-compatibly from cc-channel-octo.
safety/       prompt-injection defense: SanitizeDisplayName / Escape{Role,Section}
              + SafeText choke-point + SecurityPrefix. Ported from prompt-safety.ts.
groupctx/     per-channel group context window + cursor + @mention resolution;
              renders the [Recent group messages] delta for injection.
config/       two-layer bot-first config (~/.xclaw): global + per-bot, derived
              dirs, SOUL.md, slug + SSRF validation.
fixtures/     recorded stream-json turn (text + tool_use + result)
```

## Run

```bash
go test ./...                            # drivers, store, router, gateway, control, im/octo
go run ./cmd/xclawd                      # REPL on stdin, claude driver
go run ./cmd/xclawd -driver codex        # codex app-server driver
go run ./cmd/xclawd -control /tmp/xclaw.sock           # serve control bus (GUI)
go run ./cmd/xclawd -octo-api https://octo.example -octo-token bf_xxx   # Octo IM bot
# in the REPL: type a message; /reset clears the session; Ctrl-D exits

# cross-compile the daemon (zero cgo)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/xclawd ./cmd/xclawd
```

## Notes / next

- **Auth:** this machine's `claude` returns 401 on direct Anthropic calls (its
  key is Claude-Code-internal, not for standalone CLI) and `codex` is not logged
  in, so live turns produce no assistant text here. The full pipeline — spawn,
  protocol handshake, event normalization, routing, locking, persistence,
  multi-turn resume — is verified regardless (unit tests + live handshakes). A
  real key / login lights up the assistant-text path end-to-end.
- **Next:**
  - Control bus (`../proto`): expose the gateway over NDJSON-over-UDS so the
    Swift app (`../app`) can drive bots and render the event stream.
  - IM layer: a `Gateway`-fronting connector (e.g. Octo/WuKongIM) that produces
    `router.InboundMessage` and consumes the `Sink`.
  - Port remaining cc-channel logic: group context, cron, prompt-safety,
    skills, config (bot-first layout).
  - More drivers (e.g. Gemini) — each just implements `agent.Driver`.
