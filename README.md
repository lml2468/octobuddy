# XClaw

A cross-platform agent gateway, structured as a monorepo.

XClaw **drives coding agents (Claude, Codex, …) by spawning their CLI /
app-server and normalizing their output into one unified event stream** —
replacing the Node-only `claude-agent-sdk`. The Go core compiles to a single
static binary on every platform; native shells (starting with a macOS app) sit
on top via a control bus.

## Repository layout

```
core/     Go core — the agent gateway daemon (`xclawd`).
          driver abstraction, SQLite store, router, gateway. Single static
          binary, zero cgo, cross-compiles to mac/linux/windows.
            core/cmd/xclawd   daemon entry point
            core/agent        Driver abstraction + Claude/Codex drivers
            core/store        SQLite persistence (sessions, messages, resume map)
            core/router       per-session locking + sessionKey + rate limiting   (WIP)
            core/gateway      handleMessage orchestration pipeline               (WIP)

app/      Swift macOS app (SwiftPM package) — the native control plane / GUI.
          Talks to xclawd over the control bus. Mirrors Open Island's
          4-target structure. (scaffold)

proto/    Control-bus contract shared by core and app: the NDJSON envelope
          schema (events out of xclawd, commands in). Language-neutral.

scripts/  Build / packaging (e.g. embedding the signed xclawd binary into the
          .app bundle, notarization, DMG).
```

## Why a monorepo

The Go core and the Swift shell evolve together against one contract (`proto/`).
A monorepo keeps the daemon, the app, and their shared protocol versioned in
lockstep — a control-bus change touches all three in a single commit.

## Status

- ✅ `core/agent` driver abstraction proven across two very different agent
  protocols (Claude one-shot stream-json; Codex long-lived JSON-RPC).
- ✅ `core/store` SQLite persistence (sessions / messages / resume map, 7-day TTL).
- ✅ `core/router` + `core/gateway` — cc-channel's gateway pipeline ported
  (per-session lock, rate limiting, mention gate, resume continuity).
- ✅ `core/control` + `app/XClawCore` — control bus live end-to-end: the Swift
  client connects over the Unix socket, sends commands, and renders the agent
  event stream broadcast by the Go core.
- ✅ `app/XClawApp` — AppModel + CoreSupervisor + MenuBar/console GUI: the app
  spawns & supervises `xclawd`, connects the bus, and streams sessions live.
- ✅ `core/im/octo` — Octo IM connector: WuKongIM binary protocol (curve25519 DH
  + MD5→AES-128-CBC, key derivation verified byte-identical to cc-channel) + REST;
  wired into `xclawd` via `-octo-api`/`-octo-token`.
- ✅ `core/safety` + `core/groupctx` — prompt-injection defense (SafeText
  choke-point, security prefix, current-message anchor) and per-channel group
  context window; wired into the gateway (group turns inject a sanitized
  [Recent group messages] delta + frozen system prompt).
- ✅ `core/config` — two-layer bot-first config (~/.xclaw): global + per-bot,
  derived dirs, SOUL.md, slug + SSRF validation. Loaded by `xclawd -config`,
  which runs every configured bot in its own isolated stack (multi-bot).
- 🚧 cron (deferred); packaging.
- 🚧 packaging: bundle the signed `xclawd` into the `.app` (Helpers/), Sparkle.

## Build

```bash
# Go core
cd core && go build ./... && go test ./...

# cross-compile the daemon (zero cgo)
cd core && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/xclawd ./cmd/xclawd

# Swift app (once scaffolded)
cd app && swift build
```
