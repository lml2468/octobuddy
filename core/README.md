# OctoBuddy core (`octobuddy-daemon`)

The OctoBuddy gateway daemon, written in Go. It **drives coding agents by spawning
their CLI and normalizing their output into one unified event stream** —
replacing the Node-only `claude-agent-sdk`. Single static binary, zero cgo,
cross-compiles everywhere. Native shells (the Wails v3 desktop app in `../desktop`)
sit on top via the control bus (`../proto`).

One driver ships today — **Claude** — behind the `agent.Driver` abstraction.
That abstraction is the point: a second agent (Codex, Gemini, …) re-enters as
another `Driver` implementation without touching the gateway, router, store,
or control bus.

End-to-end pipeline: `inbound → router (mention gate + bot-loop guard +
sessionKey + rate limit + per-session lock) → store + groupctx → gateway →
agent driver → sink (IM REST + control-bus events)`, with SQLite-backed
sessions/messages and per-session resume continuity that survives daemon
restart.

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
(`--permission-mode bypassPermissions`): there is no terminal to answer
approval prompts, so any other mode would hang the turn. Bypass mode grants
every tool, so no `--allowedTools` flag is passed (claude 2.1+ rejects `*`
in allow rules).

## Layout

```
cmd/octobuddy-daemon/  daemon entry point — wires store+router+gateway+driver; ships a
              REPL inbound source (stdin) for headless / single-bot dev.
agent/        Driver abstraction (the heart). Everything above depends only on this.
  agent.go    AgentEvent, Request, Capabilities, Driver interface
  claude.go   ClaudeDriver: spawn claude CLI, parse stream-json → AgentEvent
store/        SQLite persistence: sessions + messages + resume map + token usage
              (no TTL — sessions are persistent; the daemon only reaps idle
              in-memory router locks/rate-limit buckets).
router/       sessionKey derivation (DM=per-peer, group=per-channel), per-session
              serial lock, 3-bucket rate limit, mention/bot-loop gating.
gateway/      turn pipeline: route → store → groupctx → sandbox → attachments →
              buildSystemPrompt → driver → sink → persist.
control/      control bus: NDJSON-over-UDS server + gateway EventSink (GUI clients).
              Includes a session.upserted push event so the sidebar stays in sync
              without polling sessions.list.
im/octo/      Octo IM connector: WuKongIM binary protocol (curve25519 DH + MD5→
              AES-128-CBC) + REST; inbound → router, replies via REST. Carries an
              in-connector name cache that resolves DM/peer + group/thread names
              via /v1/bot/user/info, /v1/bot/groups/{id}, /v1/bot/groups/{g}/threads/{s}.
              Ported wire-compatibly from cc-channel-octo / openclaw-channel-octo.
safety/       prompt-injection defense: SanitizeDisplayName / Escape{Role,Section}
              + SafeText choke-point + SecurityPrefix. Ported from prompt-safety.ts.
safepath/     lexical path containment + symlink defense — the single boundary
              every local-file read/write in the daemon routes through (config,
              skills, workflows, GROUP.md, sandbox cwd, IM media downloads).
sandbox/      per-session deterministic cwd + auto-memory dir under each bot's
              data root; SHA-derived names keep sessions isolated on disk.
groupctx/     per-channel group context window + cursor + @mention resolution;
              renders the [Recent group messages] delta for injection.
groupmd/      operator-trusted per-channel `<channelId>.md` loader (injected as
              [Group instructions] when the bot's groupConfigDir is set).
persona/      OBO persona-clone reply voice (openclaw on_behalf_of relay) — the
              clone speaks for a grantor while routing replies under their uid.
cron/         per-bot scheduled tasks; owner-gated create/list/delete over the
              control bus, fires synthetic CronFire inbound messages.
config/       single-file config (~/.octobuddy/config.json): shared defaults +
              inline bots[], derived data dir, SOUL.md + AGENTS.md prompt,
              slug + SSRF validation.
fixtures/     recorded stream-json turn (text + tool_use + result)
```

## Run

```bash
go test ./...                            # driver, store, router, gateway, control, im/octo
go run ./cmd/octobuddy-daemon                      # REPL on stdin, claude driver
go run ./cmd/octobuddy-daemon -control /tmp/octobuddy.sock           # serve control bus (GUI)
go run ./cmd/octobuddy-daemon -octo-api https://octo.example -octo-token bf_xxx   # single Octo IM bot
go run ./cmd/octobuddy-daemon -config                # multi-bot from ~/.octobuddy/config.json
go run ./cmd/octobuddy-daemon -config ./my.json      # multi-bot from a given config
# in the REPL: type a message; /reset clears the session; Ctrl-D exits

# cross-compile the daemon (zero cgo)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/octobuddy-daemon ./cmd/octobuddy-daemon
```

## Config mode

`-config` loads the single `~/.octobuddy/config.json` (see `config.example.json`) and
runs every bot in its bots[] list in its own isolated stack — separate SQLite
store, gateway, driver, group-context, and Octo connector, each under
`~/.octobuddy/<id>/`. Layout:

```
~/.octobuddy/
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

