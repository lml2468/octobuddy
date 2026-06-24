# Architecture

This is the navigation guide. The deep invariants — turn ordering, prompt
safety, persona OBO, sandbox layout — live in
[CLAUDE.md](CLAUDE.md). Read this first; jump there when you need the
why behind a specific seam.

## Three pieces, one wire contract

```
┌──────────────────────────────┐        ┌──────────────────────────────┐
│  desktop/  (Wails v3 + Svelte)│       │  core/   (Go daemon)         │
│  ─────────────────────────── │        │  ──────────────────────────  │
│  Svelte UI ── octobuddyservice ├──UDS─▶│  control bus (NDJSON)        │
│      (lib/store.svelte.ts)    │       │      ▲                       │
│                              │        │      │                       │
│  desktop/internal/           │        │  gateway ── router           │
│   • configstore (JSON IO)    │        │      │                       │
│   • secrets    (OS keychain) │        │  trigger.Classifier          │
│   • windowstate              │        │      │                       │
│   • workspace (read-only fs) │        │  agent.Driver ── claude CLI  │
│                              │        │      │                       │
│                              │        │  store (SQLite, no cgo)      │
│                              │        │      │                       │
└──────────────────────────────┘        │  im/octo ◀── WuKongIM        │
                                        └──────────────────────────────┘
                ▲                                       ▲
                │                  proto/               │
                └─────────── language-neutral ──────────┘
                            NDJSON envelopes
```

Three modules version together against one contract:

- **`core/`** — Go daemon `octobuddy-daemon`. Single static binary, zero cgo,
  cross-compiles to mac/linux/windows. Owns all agent-driving + IM I/O. No
  Wails import anywhere (verified by `grep -r wails core/` returning zero).
- **`desktop/`** — Go + Wails v3 (Svelte 5 + TS frontend) desktop app, macOS
  first-class, Linux/Windows supported. Never talks to Claude directly —
  spawns + drives `octobuddy-daemon` over a Unix socket.
- **`proto/`** — the language-neutral control-bus contract. Spec lives in
  `proto/README.md`; Go types live in `core/control/wire` (a
  dependency-free leaf that both sides import).

## Three layers inside the daemon

The agent gateway is a pipeline. Each box below depends only on the box to
its left:

```
inbound IM message
      │
      ▼
┌─────────────────┐     ┌──────────────────┐     ┌────────────────┐
│ im/octo         │ ──▶ │ trigger          │ ──▶ │ router         │
│  (wire decode)  │     │  Classifier      │     │  rate-limit    │
│                 │     │  → TriggerDecision     │  per-session   │
│  → Canonical    │     │   {Reason,Source,│     │  lock          │
│    Inbound      │     │    Routing,...}  │     │  ↓             │
└─────────────────┘     └──────────────────┘     │  Handle        │
                                                  │   ↓            │
                                                  │  store.Append  │
                                                  │   ↓            │
                                                  │  driver.Query  │
                                                  │   ↓ events     │
                                                  │  sink.OnEvent  │
                                                  └────────────────┘
                                                          │
                                                          ▼
                                                  reply via im/octo
                                                  (and broadcast via
                                                   control bus → desktop)
```

- **`core/im/<adapter>`** is the IM-specific edge. The default and only
  adapter today is `core/im/octo` (WuKongIM). Adding Slack/Discord means
  one new adapter, nothing else moves.
- **`core/trigger`** is the IM-agnostic decision: "should the bot reply
  to this message, and for what reason?". `Classifier.Classify(canonical, policy) → TriggerDecision`.
  Replaces the boolean `Mentioned`/`CronFire` flags that caused issue #105.
- **`core/router`** owns rate limiting, per-session serialization, and
  blocklist/bot-loop guards. Consumes `TriggerDecision`, does NOT produce
  it.
- **`core/gateway`** is the single dispatcher. `Handle(msg)` decides
  reply-vs-observe-vs-drop, runs `runTurn` under the per-session lock for
  replies, calls `Observe` inline for background context. The single
  Observe entry point per issue #105.
- **`core/agent`** is the `Driver` abstraction. `claude.go` is the only
  concrete driver today; adding Codex/Gemini means one new driver, nothing
  else moves. **Everything downstream depends only on `AgentEvent` vocabulary.**

## Data flow (inbound → outbound)

1. WuKongIM frame arrives on the WS socket (`core/im/octo/connector_run.go`)
2. Decoded to `BotMessage`, projected to `trigger.CanonicalInbound`
3. `trigger.DefaultClassifier.Classify` returns a `TriggerDecision`
   (Reason: explicit_bot / persona_grantor / observation / cron / …)
4. Connector either:
   - `gw.Handle(msg)` for reply-warranting decisions (queued per session)
   - `gw.Observe(msg)` inline for `ReasonObservation` (no lock, fast path)
   - drops silently for `ReasonOBOIrrelevant` (R10 leak guard)
5. `gateway.Handle` → router gates (rate-limit, size, blocklist) → `runTurn`
6. `runTurn` builds the prompt (system + roster + group context delta),
   resolves sandbox cwd, calls `driver.Query(req)`
7. Driver streams `AgentEvent`s; each event goes through the `Sink`
   interface to both the IM connector (typing/tool-progress/reply) AND
   the control-bus event sink (desktop GUI mirror)
8. Final reply persisted to `store`, resume id saved, reply seq advanced

## Desktop ↔ daemon boundary

The desktop **never** talks to Claude or IM directly. Every action is one
of:

- A **command** sent over the control-bus UDS (`session.send`,
  `bots.list`, `cron.create`, etc.) — request/response.
- An **event** received from the daemon (`session.text`, `session.reply`,
  `bot.status`, `error`, etc.) — push.

`desktop/octobuddyservice.go` is the Wails-bound bridge. It:
1. Spawns `octobuddy-daemon -control /tmp/octobuddy-<uid>.sock -exit-with-parent`
2. Dials the control socket
3. Forwards every envelope to the frontend as the `octobuddy:event` Wails event
4. Exposes Go-bound methods (Send, GroupsList, OctoCliRelogin, ...) that
   marshal arguments + push them as control-bus commands
5. Restarts the daemon on crash with exponential backoff

`desktop/internal/` holds the thin services the bridge needs:
- `control` — the UDS NDJSON client (Go side of `core/control/wire`)
- `configstore` — read/write `~/.octobuddy/config.json` (+ per-bot SOUL.md /
  AGENTS.md)
- `secrets` — OS keychain (macOS Keychain / Windows Credential Manager /
  Linux libsecret via `go-keyring`, zero cgo)
- `skills`, `workflows` — per-bot CRUD over
  `~/.octobuddy/<id>/.claude/{skills,workflows}/` bundles
- `workspace` — read-only sandbox tree + file preview
- `windowstate` — persisted console window bounds
- `autostart` — macOS LaunchAgent (no-op on other platforms)
- `octocli` — bundle/install/upgrade the octo-cli companion

## Where invariants live

| Invariant | Source of truth |
|---|---|
| sessionKey derivation (DM = per-peer, Group = per-channel) | `core/router/router.go::InboundMessage.SessionKey` |
| Per-session serial turn execution | `core/router/locks.go` + `RouteAndHandle` |
| Trigger decision (single classifier) | `core/trigger/default.go` |
| Group-context delta ordering (delta-before-push) | `core/gateway/gateway_prompt.go::buildGroupPrompt` |
| Prompt safety prefix + escaping | `core/safety/` |
| OBO trust gate (signed by configured grantor) | `core/trigger/obo.go::evaluateOBOTrust` |
| Single Observe entry point | `core/gateway/gateway.go::Handle` |
| Sandbox cwd partitioning | `core/sandbox/` + `core/safepath/` |
| Wire schema (Go ↔ JS) | `core/control/wire/events.go` |

When in doubt, follow the import: if a thing is imported from `core/`, it's
domain logic; if from `desktop/internal/`, it's a Wails-side adapter; if
from `wailsapp/wails`, the file is on the desktop edge.

## Adding things

- **A new IM adapter** (Slack, Discord, …): implement `gateway.Sink` +
  produce `router.InboundMessage` with a `*trigger.TriggerDecision`. Cite
  parity with `core/im/octo`.
- **A new agent driver** (Codex, Gemini, …): implement `agent.Driver` +
  emit `AgentEvent`s. Cite parity with `core/agent/claude.go`.
- **A new desktop service**: add `desktop/internal/<name>/` with a
  testable Go API, wire into `octobuddyservice.go` as a thin bridge. Keep
  business logic OUT of the bridge.
- **A new wire field**: extend `core/control/wire/events.go` + the
  matching consumer in `desktop/frontend/src/lib/store.svelte.ts`. If the
  field is persistent state, also extend `core/store/` + add a migration.
