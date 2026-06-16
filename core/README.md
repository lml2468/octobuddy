# XClaw core (`xclawd`)

The XClaw gateway daemon, written in Go. It **drives coding agents by spawning
their CLI and normalizing their output into one unified event stream** —
replacing the Node-only `claude-agent-sdk`. Single static binary, zero cgo,
cross-compiles everywhere. Native shells (the Wails v3 desktop app in `../desktop`)
sit on top via the control bus (`../proto`).

Phase 1 ships a single driver — **Claude** — behind the `agent.Driver`
abstraction. The abstraction is the point: a second agent (Codex, Gemini, …)
re-enters as another `Driver` implementation without touching the gateway,
router, store, or control bus.

> Status: headless MVP runs end-to-end — `inbound → router (lock + gate +
> rate limit) → gateway → agent driver → outbound sink`, with SQLite-backed
> sessions/messages and per-session resume continuity.

## What this proves

| Claim | How it's proven | Result |
|---|---|---|
| The `claude` CLI can replace `claude-agent-sdk` | `agent.ClaudeDriver` spawns `claude -p --output-format stream-json --verbose [--resume …]` and normalizes its line-delimited JSON | ✅ live spawn + parse verified against real CLI output (`system`/`init`/`api_retry` lines + extracted session id) |
| Agent output normalizes to one unified stream | `agent.AgentEvent` + the driver translator | ✅ unit tests on recorded fixtures (no API key needed) |
| Resume/session continuity works without the SDK | `store.Store` (pure-Go SQLite) maps `sessionKey → resume_id`; gateway persists then resumes | ✅ verified by `gateway` tests (second turn carries the first turn's resume id) |
| Go core = single static binary, trivial cross-compile | `CGO_ENABLED=0 go build` to 5 platforms | ✅ darwin/{arm64,amd64}, linux/{amd64,arm64}, windows/amd64 — all ~10MB, zero cgo |

Everything downstream of `agent.Driver` — router, store, gateway `consume()`,
control bus — depends only on the `AgentEvent` vocabulary, never on Claude
specifics. That is the multi-agent foundation: adding a driver is additive.

## Driver

| Driver | Mechanism | Native protocol |
|---|---|---|
| `ClaudeDriver` | `claude -p --output-format stream-json` | line-delimited JSON over stdout (one-shot) |

`ClaudeDriver` satisfies `agent.Driver`. Headless invariants are baked in
(`--allowedTools * --permission-mode bypassPermissions`): there is no terminal
to answer approval prompts, so any other mode would hang the turn.

## Layout

```
cmd/xclawd/   daemon entry point — wires store+router+gateway+driver; ships a
              REPL inbound source (stdin) for the headless MVP.
agent/        Driver abstraction (the heart). Everything above depends only on this.
  agent.go    AgentEvent, Request, Capabilities, Driver interface
  claude.go   ClaudeDriver: spawn claude CLI, parse stream-json → AgentEvent
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
config/       single-file config (~/.xclaw/config.json): shared defaults +
              inline bots[], derived data dir, SOUL.md + AGENTS.md prompt,
              slug + SSRF validation.
fixtures/     recorded stream-json turn (text + tool_use + result)
```

## Run

```bash
go test ./...                            # driver, store, router, gateway, control, im/octo
go run ./cmd/xclawd                      # REPL on stdin, claude driver
go run ./cmd/xclawd -control /tmp/xclaw.sock           # serve control bus (GUI)
go run ./cmd/xclawd -octo-api https://octo.example -octo-token bf_xxx   # single Octo IM bot
go run ./cmd/xclawd -config                # multi-bot from ~/.xclaw/config.json
go run ./cmd/xclawd -config ./my.json      # multi-bot from a given config
# in the REPL: type a message; /reset clears the session; Ctrl-D exits

# cross-compile the daemon (zero cgo)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/xclawd ./cmd/xclawd
```

## Config mode

`-config` loads the single `~/.xclaw/config.json` (see `config.example.json`) and
runs every bot in its bots[] list in its own isolated stack — separate SQLite
store, gateway, driver, group-context, and Octo connector, each under
`~/.xclaw/<id>/`. Layout:

```
~/.xclaw/
  config.json          # apiUrl + shared defaults + bots[] (each bot: id + octoToken + overrides)
  <id>/SOUL.md         # per-bot identity/persona (operator-trusted system prompt)
  <id>/AGENTS.md       # per-bot behavior norms (appended after SOUL.md)
  <id>/data/           # derived, per-bot isolated SQLite + state
```

Each `bots[]` entry holds `id` + `octoToken` and may override the top-level
`apiUrl`/`agent`/`rateLimit`/`context` defaults. The system prompt is file-based,
not a config field: SOUL.md (who the bot is) followed by AGENTS.md (how it should
behave) are concatenated and passed to the agent as the operator-trusted prompt.
Either file may be omitted.

Secrets (`octoToken`, `agent.gatewayToken`) are **optional** in the file: they
can be injected at runtime over the control bus (`secret.inject`) — the macOS app
keeps them in the Keychain. A bot started without a token waits ("awaiting
secret") until one is injected, then connects. Put tokens in the file only for
headless/no-GUI deployments.

## Notes / next

- **Auth:** this machine's `claude` returns 401 on direct Anthropic calls (its
  key is Claude-Code-internal, not for standalone CLI), so live turns produce no
  assistant text here. The full pipeline — spawn, event normalization, routing,
  locking, persistence, multi-turn resume — is verified regardless (unit tests +
  live spawn). A real key / model gateway lights up the assistant-text path
  end-to-end.
- **Next:**
  - More drivers (e.g. Codex, Gemini) — each just implements `agent.Driver`,
    additive to the gateway. Codex's `app-server` is a long-lived JSON-RPC duplex
    process — a different protocol shape that still reduces to the same
    `AgentEvent` stream, which is the abstraction's real stress test.

