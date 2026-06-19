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

The trust boundary from the peer-cred + chmod layers is "same OS user as the
daemon." That is necessary but **not sufficient**: the daemon spawns the agent
CLI as the *same uid*, so a prompt-injected agent (which has a `Bash` tool)
could hand-craft NDJSON and dial the socket. The peer-cred check cannot tell the
operator's GUI from the agent's CLI. A second layer closes that gap:

- **Capability token (GUI-only).** At spawn the launcher (the GUI) mints a random
  token and hands it to the daemon **out-of-band** — over the daemon's stdin, a
  private pipe the launcher owns — so it never appears in an env var or argv
  (both world-readable via `/proc/<pid>/`) and the spawned agent, which the daemon
  launches with its own fresh stdin, never inherits the fd. The daemon holds it in
  memory only (never logged, never written to `config.json`). A client proves it is
  the GUI by presenting the token in an `auth` command (constant-time compared);
  that marks the connection authorized for the **privileged** command set. The
  token is required: a daemon with no token configured (bare CLI/dev) can
  authenticate no one, so every privileged command is denied (fail closed).

  - **Privileged (require auth):** `session.send`, `session.reset`,
    `session.history`, `sessions.list`, `usage.stats`, `secret.inject`,
    `cron.create`, `cron.list`, `cron.delete`, and the broadcast **event stream**
    (it carries every session's live activity — cross-session disclosure). These
    are operator/GUI→daemon operations with no sanctioned agent path. Note in
    particular that `session.history`, `sessions.list`, and `cron.list` are
    privileged: their handlers take an attacker-influenceable `botId`/`sessionKey`
    and apply no per-session scoping, so an injected same-uid agent could
    otherwise read any session's history or enumerate the owner's scheduled
    prompts. They are the static twin of the event-stream broadcast gate.
  - **Open (no auth):** `health`, `bots.list` — low-value liveness/roster metadata
    only.

A body field is **never** an authorization claim (see the cron owner-gate below);
authorization is the peer-cred uid + the capability token, never client-asserted
data.

> **Residual risk (tracked separately):** same-uid is a weak boundary — a
> determined injected agent with `ptrace`/`/proc/<pid>/mem` access (where Yama
> `ptrace_scope` permits) could still extract the in-memory token. The durable fix
> is privilege separation: run the agent CLI under a dedicated lower-privileged
> uid so the peer-cred gate becomes a real boundary and the token is defense in
> depth. Out of scope here.

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
| `auth` | `{token}` | `{ok}` (handshake; marks the connection authorized for privileged commands — see Access control) |
| `bots.list` | — | `[{id, connected, lastError}]` |
| `health` | — | `{uptime, driver, bots, connections}` |
| `session.send` | `{botId?, uid, text}` | `{ok}` (turn streamed via events) |
| `session.history` | `{botId?, sessionKey, limit}` | `{botId, key, messages: [{role, content, ts}]}` (echoes botId+key so the client routes rows to the right session even if the user switched mid-fetch) |
| `sessions.list` | `{botId?}` | `{botId, sessions: [{key, channelType, updatedAt, preview, lastRole}]}` |
| `usage.stats` | `{botId?, since?}` | `{inputTokens, outputTokens, cachedInputTokens, cacheCreationInputTokens, costUsd, turns}` (cumulative; `since` is a Unix-seconds lower bound, omitted = all time) |
| `session.reset` | `{botId?, uid}` | `{ok}` (clears the resume mapping) |
| `secret.inject` | `{botId?, kind, value, clear?}` | `{ok}` (held in memory; never persisted. `clear:true` removes the token for `kind`; otherwise an empty `value` is ignored so seeding can't clobber an injected token) |
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
