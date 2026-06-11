# XClaw control-bus protocol

The contract between the Go core (`xclawd`) and any client shell (the Swift
macOS app, a web console, a CLI). It is intentionally language-neutral so each
side can implement it independently.

## Transport

- **v1:** newline-delimited JSON (NDJSON) over a Unix domain socket.
  - `xclawd` listens; clients connect.
  - Each line is one envelope. UTF-8. Max frame size enforced by the server.
  - Rationale: trivial to implement in Swift (`Network`/`FileHandle`) and Go
    (`bufio`), easy to debug (`socat`/`nc`), and battle-tested in Open Island's
    bridge. Upgrade path to gRPC/protobuf-over-UDS if the schema grows.

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

`session.send` routes `uid` as a DM inbound. A non-`ok` outcome (router drop or
turn error) is reported asynchronously as an `error` event, since the response
returns immediately and the turn streams back.

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
