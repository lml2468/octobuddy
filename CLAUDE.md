# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

XClaw is a cross-platform **agent gateway**: it drives coding-agent CLIs (Claude
first) by spawning them and normalizing their output into one unified event
stream — replacing the Node-only `claude-agent-sdk`. Monorepo of three pieces
that version together against one contract:

- `core/` — Go daemon `xclawd` (the gateway). Single static binary, **zero cgo**,
  cross-compiles to mac/linux/windows.
- `desktop/` — **Go + Wails v3** desktop app (Svelte + TS frontend, macOS/Win/Linux).
  A control-bus client; never talks to Claude directly — it spawns + drives
  `xclawd`. The UI is a clean **WeChat/iMessage-grade** chat UI (CSS/SVG), not
  native chrome. Its Go backend reuses the wire contract directly.
- `proto/` — the language-neutral control-bus contract (NDJSON envelopes over a
  Unix socket) shared by core and the app. Spec lives in `proto/README.md`; the
  Go types live in `core/control/wire` (a dependency-free leaf both sides import).

The repo is a **Go workspace** (`go.work`) tying `./core` and `./desktop`. The
desktop module is `github.com/lml2468/xclaw/desktop` and pulls `core` in via a
local `replace`.

## Commands

```bash
# Go core (run from core/)
cd core && go build ./... && go test ./...
go test ./gateway/ -run TestName        # single package / single test
go run ./cmd/xclawd                       # REPL on stdin (type a msg; /reset; Ctrl-D)
go run ./cmd/xclawd -control /tmp/xclaw.sock   # serve control bus for GUI clients
go run ./cmd/xclawd -config               # multi-bot from ~/.xclaw/config.json
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/xclawd ./cmd/xclawd  # cross-compile

# Desktop app (Wails v3 — needs the wails3 CLI:
#   go install github.com/wailsapp/wails/v3/cmd/wails3@latest)
cd desktop && go build ./... && go vet ./...
cd desktop/frontend && npm run build && npx svelte-check   # frontend build + typecheck
zsh scripts/run-dev.sh                     # build core + `wails3 dev` (needs ~/.xclaw/config.json)
zsh scripts/run-dev.sh --seed-config       # write a starter config first
zsh scripts/run-dev.sh --preview           # UI preview: mock data, no daemon (XCLAW_PREVIEW)

# Package a distributable XClaw.app (+ .zip); embeds xclawd, signs inside-out.
# ad-hoc by default; pass the identity to Developer-sign, a profile to notarize.
XCLAW_SIGN_IDENTITY="Apple Development: …" zsh scripts/package-desktop.sh
```

The desktop GUI's own visual-iteration loop is **preview mode**: launch the built
binary with `XCLAW_PREVIEW=1` (optional `XCLAW_PREVIEW_THEME=dark|light`,
`XCLAW_PREVIEW_EDITOR=1`) — it seeds a mock roster + transcript and skips the
daemon, so the watercolor UI can be screenshotted without a live bot.

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
inbound → router (mention/免@ gate → bot-loop guard → sessionKey → size gate → rate limit → per-session lock)
        → store.GetOrCreate → load resume id → groupctx backfill + answered/new segmentation
        → materialize attachments into cwd → buildSystemPrompt (SecurityPrefix + SOUL/AGENTS + roster + GROUP.md + persona)
        → driver.Query → stream AgentEvents → sink.OnEvent (typing heartbeat / opt-in tool-progress) → assemble reply
        → persist assistant text + resume id + reply cursor → sink.OnReply (mention resolution / persona voice)
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
- **Skills** (`core/sandbox/skill.go`, ported from `skill-linker.ts`): each turn
  symlinks operator skill bundles into the session sandbox's `.claude/skills/`
  so the agent CLI discovers them. Sources are `[]SkillSource{Dir,Allow}` in
  ascending precedence — the **global catalog** `~/.xclaw/skills/<name>/` filtered
  by the bot's `Skills` allow-list (`gateway.WithSkillAllow`, from `Resolved.Skills`;
  nil/empty = none), then the **per-bot dir** `~/.xclaw/<id>/skills` (unfiltered,
  always links, shadows global by name). Managed from the desktop Skills window.
- **ClaudeDriver headless invariants** (`core/agent/claude.go`): always spawns
  `claude -p --output-format stream-json --verbose --include-partial-messages --permission-mode bypassPermissions`.
  Bypass is mandatory — there is no terminal to answer approval prompts, so any
  other mode hangs the turn; it also grants every tool, so no `--allowedTools` is
  passed (claude 2.1+ rejects `*` in allow rules). `--include-partial-messages`
  gives token-level streaming: the driver parses `stream_event` deltas and
  suppresses the duplicate complete block. Tool/permission policy is
  intentionally NOT in `agent.Request`; it is a fixed claude-only invariant.
- **Feature modules layered on the pipeline** (each cites its TS source in its
  package doc): `core/cron/` — per-bot scheduled tasks, owner-gated
  `cron.create/list/delete` over the control bus; `core/groupmd/` — operator
  `<channelId>.md` → trusted `[Group instructions]`; `core/persona/` — OBO
  persona-clone reply voice. Inbound media/markers, outbound @mention
  resolution, threads, and typing/tool-progress all live in `core/im/octo/`.

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
  `apiUrl`/`agent`/`rateLimit`/`context` defaults. Capability switches live under
  `agent` (`cron`, `toolProgress`); the group-gating lists (`mentionFreeGroups`,
  `knownBotUids`, `allowedBotUids`, `botBlocklist`) plus `groupConfigDir`,
  `onBehalfOf`, and `skills` (the per-bot skill allow-list, names from the global
  `~/.xclaw/skills/` catalog) are top-level defaults a bot may override — a per-bot
  value REPLACES the default. `core/config.example.json` is the canonical field list.
- `core/config/` does slug + SSRF validation on URLs — keep that on any new
  config field that holds a URL. `groupConfigDir` files are injected UNSANITIZED
  as `[Group instructions]`, so config load rejects a dir at/under a bot's
  `cwdBase` (else a user-driven agent could write its own future instructions).

## IM connector

`core/im/octo/` speaks the WuKongIM binary protocol (curve25519 DH + MD5→
AES-128-CBC key derivation, verified byte-identical to the upstream cc-channel
reference) plus REST. Inbound → router; replies go out via REST. It is one
connector behind the agent/IM-agnostic `router.InboundMessage` — the gateway
neither knows nor cares which IM is attached.
Beyond plain text it renders non-text payloads to markers, materializes inbound
media/files into the session cwd, resolves outbound @mentions, runs the OBO
persona relay + thread routing, and emits a 5 s typing heartbeat.

## Desktop app (`desktop/`)

A thin control-bus client (Wails v3 alpha + Svelte 5/TS); the daemon holds all
logic, so swapping the GUI never touches `core/`.

- **Go backend**: `main.go` (app + window + system tray + single-instance);
  `xclawservice.go` is the Wails-bound bridge — spawns `xclawd`, dials the control
  socket, forwards every envelope to the frontend as the `xclaw:event` Wails event,
  exposes command/config methods, and auto-reconnects on daemon crash.
  `internal/`: `control` (UDS/NDJSON client over `core/control/wire`), `core`
  (supervisor: resolve binary → spawn `-control … -exit-with-parent` → stop/restart),
  `configstore` (read/write `~/.xclaw/config.json` + per-bot SOUL/AGENTS + skill
  allow-list), `skills` (CRUD over the `~/.xclaw/skills/` catalog bundles, with
  slug + path-traversal validation), `octocli` (bundle/install/upgrade the
  octo-cli companion), `secrets` (tokens in the OS credential store via go-keyring,
  zero cgo; injected at runtime, **never** written to config.json).
- **Frontend** (`frontend/src`): `lib/store.svelte.ts` is the single reducer —
  it folds `xclaw:event` envelopes into bots/sessions/messages and owns the
  rAF typewriter/coalescing. Components in `lib/components/` (Rail · Conversations
  · Transcript · Bubble · Composer · ConfigEditor · SkillsPanel · Avatar); tokens in
  `lib/styles/theme.css`.
- **Edit Bots / Manage Skills are sibling modals** (`ConfigEditor` · `SkillsPanel`),
  both opened over the console from the rail gear menu (and tray) via
  `xclaw:open-editor` / `xclaw:open-skills` events — same scrim + centered card +
  header/✕ chrome (keep them visually consistent). SkillsPanel is a multi-file
  bundle editor; ConfigEditor has the per-bot "Available skills" checklist.
  NOTE: `window.confirm/alert` are no-ops in the Wails webview — use an in-app
  dialog for any confirmation.
- **Design direction (committed — do not re-pivot)**: clean WeChat/iMessage-grade
  chat UI. Dark bot-rail → conversation list → chat; green accent (`#07c160`),
  green selected rows + outgoing bubbles, square-rounded avatars, **Geist** (Sans
  for UI, Mono for code + metadata), restrained 4–8px radii, content edge-to-edge.
  Watercolor and Liquid-Glass were both tried and rejected.
- **Verify UI by measurement, not eyeballing**: `XCLAW_PREVIEW=1` (with
  `XCLAW_PREVIEW_THEME=dark|light`, `XCLAW_PREVIEW_EMPTY=1`, `XCLAW_PREVIEW_EDITOR=1`)
  seeds mock data and skips the daemon. Run `wails3 dev`, then drive
  `http://127.0.0.1:9245/?preview=1&theme=dark` in headless Chrome via Playwright
  (`playwright-core` + `channel:"chrome"`) to screenshot and assert geometry
  (viewport fill, header alignment, overflow/clipping, tap-target size). This
  caught real bugs the eye missed (a template-CSS inset "ring", rail flex-overflow
  clipping).
- **macOS gotchas**: traffic lights overlap the leftmost pane — only the rail
  clears them (taller rail header); list/chat headers stay compact. Don't link a
  global `style.css` in `index.html` (the Vite template's `#app{max-width;padding}`
  re-insets everything) — `theme.css` is the single source of truth, imported in
  `App.svelte`. Keychain injection prompts on an unsigned/re-signed binary (allow
  once; a stable signing identity makes it stick). After changing Go binding
  signatures: `cd desktop && wails3 generate bindings -d frontend/bindings`.

## Lineage

Much of `core/` is a Go port of the TypeScript `cc-channel` / `cc-channel-octo`
gateway (the package docs cite the original files, e.g. `prompt-safety.ts`,
`group-context.ts`, `cwd-resolver.ts`). When porting more behavior, follow the
existing pattern of naming the source file in the package doc and preserving its
ordering/semantics rather than re-deriving them.
