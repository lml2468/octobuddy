# XClaw control-bus protocol

The contract between the Go core (`xclawd`) and any client shell (the Wails v3
desktop app, a web console, a CLI). It is intentionally language-neutral so each
side can implement it independently.

## Transport

- **v1:** newline-delimited JSON (NDJSON) over a Unix domain socket.
  - `xclawd` listens; clients connect.
  - Each line is one envelope. UTF-8. Max frame size enforced by the server.
  - Rationale: trivial to implement in Go (`bufio`) and TypeScript, easy to
    debug (`socat`/`nc`), and battle-tested in Open Island's
    bridge. Upgrade path to gRPC/protobuf-over-UDS if the schema grows.

### Access control

The bus exposes privileged operations (`session.send`, `secret.inject`,
`cron.*`, history, the broadcast event stream), so it must not be drivable by an
arbitrary local process. The daemon enforces an **owner-only** boundary on the
socket:

- **Peer-credential check (authoritative).** Every accepted connection's peer
  OS-uid is read from the kernel (`SO_PEERCRED` on Linux, `LOCAL_PEERCRED` on
  macOS) and must equal the daemon's effective uid; a cross-uid process is
  dropped at accept. This does not depend on filesystem perms, so it holds even
  with the socket in a world-writable `/tmp` and on platforms that ignore socket
  perms (macOS). Fail-closed: a peer-cred read error drops the connection.
  Windows AF_UNIX exposes no peer credentials → the check is skipped there and
  the socket relies on filesystem ACLs.
- **chmod 0600 (defense in depth).** The kernel enforces socket-connect perms on
  Linux, denying a different user `connect()` before the peer-cred layer runs.

The trust boundary is therefore "same OS user as the daemon." Commands carry no
per-command authentication beyond this; a body field is **never** an
authorization claim (see the cron owner-gate below).

## Envelope

```jsonc
{ "v": 1, "kind": "event" | "command" | "response", "id": "<uuid>", "ts": 1234567890, "body": { ... } }
```

- `id` correlates a `command` with its `response`.
- `event`s are unsolicited (server → client) and carry no correlation id.

## Commands (client → core)

Every command body may carry an optional `botId` to address a specific bot in
multi-bot (config) mode; it is ignored in single-bot mode.

| command | body | response |
|---|---|---|
| `bots.list` | — | `[{id, connected, lastError}]` |
| `health` | — | `{uptime, driver, bots, connections}` |
| `session.send` | `{botId?, uid, text}` | `{ok}` (turn streamed via events) |
| `session.history` | `{botId?, sessionKey, limit}` | `[{role, content, ts}]` |
| `session.reset` | `{botId?, uid}` | `{ok}` (clears the resume mapping) |
| `secret.inject` | `{botId?, kind, value}` | `{ok}` (held in memory; never persisted) |
| `cron.create` | `{botId?, schedule, prompt, recurring?, channelId?, channelType?, fromName?}` (`uid` accepted but ignored — authz + DM binding use the resolved owner) | `{id, schedule, recurring, prompt, nextRun, enabled}` |
| `cron.list` | `{botId?}` | `[{id, schedule, recurring, prompt, nextRun, enabled}]` |
| `cron.delete` | `{botId?, id}` (`uid` accepted but ignored for authz) | `{ok}` |

`session.send` routes `uid` as a DM inbound. A non-`ok` outcome (router drop or
turn error) is reported asynchronously as an `error` event, since the response
returns immediately and the turn streams back.

### Cron / scheduled tasks (#115)

Per-bot scheduled tasks, enabled by `agent.cron` in the config. Faithful port of
cc-channel-octo's cron feature (`cron-evaluator.ts`, `cron-store.ts`,
`cron-scheduler.ts`, `cron-tool.ts`). In cc-channel these surfaced as an
in-process MCP server the agent called; xclaw exposes the same create/list/delete
over the control bus instead.

- `schedule` is a **5-field cron expression** (`0 9 * * 1-5` = weekdays 9am, with
  standard dom/dow OR semantics) **or a one-shot ISO datetime** (`2026-06-09T09:00:00Z`,
  strictly validated — `Feb 30`/`hour 25`/past times are rejected).
- `recurring` defaults to `true` for cron exprs and `false` for one-shots.
- A created task **binds** to the session that created it: a `channelId` (with
  `channelType` 2) targets a group; omitting it (or `channelType` 1) targets the
  DM with `uid`. When the task fires, its `prompt` runs as a synthetic message in
  that session and the reply is delivered there.
- **Owner-gated:** `cron.create` / `cron.delete` are gated on the
  **server-resolved owner uid** (`owner_uid` from registration), never on a body
  field — the agent reaches cron over an agent-controlled CLI, so a body `uid`
  is a forgeable claim a prompt injection could set to the owner's. The body
  `uid` is accepted for proto compatibility but **ignored** for authorization
  and for DM binding (the resolved owner is used for both); a created task always
  runs as the owner. If the bot has no resolved owner yet, create/delete fail
  closed. `cron.list` is read-only. Cron fires bypass the group @mention gate and
  the rate limit (operator-scheduled, in-process); they never run
  untrusted-user-created tasks (the security prefix carries the advisory
  defense-in-depth layer).
- Caps: ≤ 50 tasks/bot, ≤ 2048-byte prompt. A bot without `agent.cron` returns an
  error for all three commands.

**Planned (not yet implemented):** `bot.start`, `bot.stop`, `config.reload`.

## Events (core → client)

Derived from the unified `agent.AgentEvent` plus gateway lifecycle. Every event
body carries an optional `botId` (empty in single-bot mode).

| event kind | body |
|---|---|
| `bot.status` | `{id, connected, lastError}` (multi-bot per-bot status) |
| `session.activity` | `{botId?, sessionKey, kind}` where kind ∈ `turnStart` \| `thinking` \| `toolResult` \| `turnDone` |
| `session.text` | `{botId?, sessionKey, delta}` (assistant token stream) |
| `session.tool` | `{botId?, sessionKey, name, params}` |
| `session.usage` | `{botId?, sessionKey, inputTokens, outputTokens}` |
| `session.reply` | `{botId?, sessionKey, text}` (final assembled reply for a turn) |
| `error` | `{botId?, scope, message, recoverable}` |

## Notes

- The `session.*` event vocabulary is a projection of `core/agent`'s
  `AgentEvent`, so the same normalization that turns a driver's native output
  into a unified stream also unifies what every shell renders. Token text is its
  own `session.text` event and tool calls their own `session.tool` event; the
  `session.activity` kinds cover the non-payload lifecycle beats.
- Secrets travel **into** the core (`secret.inject`) but never appear in any
  event or response; the core holds them in memory only.
