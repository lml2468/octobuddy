# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

XClaw is a cross-platform **agent gateway**: it drives coding-agent CLIs (Claude
first) by spawning them and normalizing their output into one unified event
stream — replacing the Node-only `claude-agent-sdk`. Monorepo of three pieces
that version together against one contract:

- `core/` — Go daemon `xclawd` (the gateway). Single static binary, **zero cgo**,
  cross-compiles to mac/linux/windows.
- `app/` — Swift macOS app (SwiftPM, macOS 14+). A control-bus client; never
  talks to Claude directly — it drives `xclawd`.
- `proto/` — the language-neutral control-bus contract (NDJSON envelopes over a
  Unix socket) shared by core and app. Spec lives in `proto/README.md`.

## Commands

```bash
# Go core (run from core/)
cd core && go build ./... && go test ./...
go test ./gateway/ -run TestName        # single package / single test
go run ./cmd/xclawd                       # REPL on stdin (type a msg; /reset; Ctrl-D)
go run ./cmd/xclawd -control /tmp/xclaw.sock   # serve control bus for GUI clients
go run ./cmd/xclawd -config               # multi-bot from ~/.xclaw/config.json
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/xclawd ./cmd/xclawd  # cross-compile

# Swift app
cd app && swift build && swift test
zsh scripts/run-dev.sh                     # build core + launch app (needs ~/.xclaw/config.json)
zsh scripts/run-dev.sh --seed-config       # write a starter config first

# Package a distributable XClaw.app (+ .zip/.dmg); ad-hoc signed by default
zsh scripts/package-app.sh
```

Go module path is `github.com/lml2468/xclaw/core` (Go 1.26). Tests need **no API
key** — they run against recorded fixtures (`core/fixtures/`) and live CLI spawns
that only assert parsing/wiring.

## Architecture: the turn pipeline

The whole point is the `agent.Driver` abstraction. **Everything downstream of it
depends only on the `agent.AgentEvent` vocabulary, never on Claude specifics** —
adding a second agent (Codex, Gemini) means writing one new `Driver`, touching
nothing else. When changing the gateway/router/store/control bus, do not reach
into driver-specific behavior; keep the dependency pointing at `agent`.

Inbound message flows through (`core/gateway/gateway.go` `runTurn`):

```
inbound → router (mention gate → sessionKey → size gate → rate limit → per-session lock)
        → store.GetOrCreate → load resume id → [group-context injection]
        → driver.Query → stream AgentEvents → sink.OnEvent → assemble reply
        → persist assistant text + resume id → sink.OnReply
```

Key invariants to preserve:

- **sessionKey derivation** (`core/router/router.go`): DM = per-peer
  (`<spaceId>:<uid>`), Group = per-channel (one shared session). Never fall back
  to `""` on an unroutable message — that would collapse unrelated peers into one
  session and leak history/memory. The router holds a **mutex-per-key** across
  the entire turn so same-session turns serialize; different sessions run
  concurrently.
- **The sandbox partition key is the SAME sessionKey** (`core/sandbox/`), prefixed
  by channel kind (`dm:` / `group:`), so a session's cwd can never drift from its
  history partition. Each session gets a deterministic hashed subdir. Note: cwd is
  a *starting* dir, not a chroot — an agent with Bash can still reach absolute
  paths; isolation across spaces is "one bot per space, separate cwdBase".
- **Resume continuity without the SDK**: `store` maps `sessionKey → resume_id`;
  the gateway persists it after a turn and passes it as `Request.SessionID` next
  turn. 7-day TTL on sessions/messages/sandboxes.
- **ClaudeDriver headless invariants** (`core/agent/claude.go`): always spawns
  `claude -p --output-format stream-json --verbose --allowedTools * --permission-mode bypassPermissions`.
  Bypass is mandatory — there is no terminal to answer approval prompts, so any
  other mode hangs the turn. Tool/permission policy is intentionally NOT in
  `agent.Request`; it is a fixed claude-only invariant.

## Security model (group chat / prompt injection)

Group turns carry untrusted text, so two modules guard the prompt — preserve
their ordering when touching the gateway:

- `core/safety/` — `SafeText` choke-point, `SecurityPrefix` (non-overridable,
  prepended), `CurrentMessageAnchor`, escaping. The system prompt append is
  always `SecurityPrefix` + operator-trusted SOUL.md/AGENTS.md, both wrapped as
  `TrustedText`.
- `core/groupctx/` — per-channel rolling context window + cursor + @mention
  resolution. **Critical ordering** (`runTurn`): build the `[Recent group
  messages]` delta BEFORE caching the current message, or it echoes into itself;
  the delta is sanitized as untrusted background and the real request is fenced
  by the current-message anchor.

## Config & multi-bot

`-config` loads a single `~/.xclaw/config.json` (see `core/config.example.json`)
and runs **every bot in `bots[]` in its own fully isolated stack** — separate
store, gateway, driver, group-context, Octo connector, each under `~/.xclaw/<id>/`.

- System prompt is **file-based, not a config field**: `<id>/SOUL.md` (identity)
  concatenated with `<id>/AGENTS.md` (behavior norms), passed as the
  operator-trusted append. Either may be omitted.
- Each `bots[]` entry is `id` + `octoToken` and may override top-level
  `apiUrl`/`agent`/`rateLimit`/`context` defaults.
- `core/config/` does slug + SSRF validation on URLs — keep that on any new
  config field that holds a URL.

## IM connector

`core/im/octo/` speaks the WuKongIM binary protocol (curve25519 DH + MD5→
AES-128-CBC key derivation, verified byte-identical to the upstream cc-channel
reference) plus REST. Inbound → router; replies go out via REST. It is one
connector behind the agent/IM-agnostic `router.InboundMessage` — the gateway
neither knows nor cares which IM is attached.

## Lineage

Much of `core/` is a Go port of the TypeScript `cc-channel` / `cc-channel-octo`
gateway (the package docs cite the original files, e.g. `prompt-safety.ts`,
`group-context.ts`, `cwd-resolver.ts`). When porting more behavior, follow the
existing pattern of naming the source file in the package doc and preserving its
ordering/semantics rather than re-deriving them.
