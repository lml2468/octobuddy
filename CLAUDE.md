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
cd desktop/frontend && npm run build && npm run check   # frontend build + typecheck (svelte-check)
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
  turn. Sessions/messages/sandboxes are **persistent** (no TTL reclamation; the
  daemon's periodic reaper only evicts idle in-memory router lock/rate-limit
  buckets). **Self-healing**: if a turn fails because the resume id is unknown
  (driver emits `AgentEvent.ResumeInvalid` — e.g. the session predates a
  config-dir change), the gateway swallows the doomed attempt, clears the
  mapping, and retries the turn fresh.
- **Skills & workflows — marketplace + per-bot install** (`core/sandbox/skill.go`,
  ported from `skill-linker.ts`): the global dirs `~/.xclaw/skills` (SKILL.md
  bundles) and `~/.xclaw/workflows` (`*.js`) are a **read-only marketplace**. A
  bot uses an asset only after it is **installed** into the bot's own dir
  `~/.xclaw/<id>/skills|workflows` — an install is a **symlink** to the catalog
  entry — and a bot may also author its own real assets there. Each turn the
  gateway symlinks **only the per-bot dir** into the session sandbox's `.claude/`
  (`LinkSkillsIntoSandbox` / `LinkWorkflowsIntoSandbox`, one collect/prune/link
  core): so a marketplace asset reaches the agent solely via the per-bot symlink,
  forming a chain `sandbox/.claude/skills/<n>` → `<id>/skills/<n>` → catalog. The
  gateway link source is single (`{Dir: g.skillsDir}` / `{Dir: g.workflowsDir}`);
  `SkillSource{Dir,Allow}` still supports an allow-list but it is **no longer
  used** — there is no global-catalog allow-list and no `skills`/`workflows`
  config field anymore (legacy keys in `config.json` are silently ignored). Install
  mechanics live in `desktop/internal/assetlib` (`Install`/`Uninstall`/`Prune`/
  `PruneInstallsAcrossBots`), shared by the desktop `skills` + `workflows`
  packages; catalog delete prunes the now-dangling install symlinks across bots,
  and `listIn` surfaces a dead symlink as `Installed+Broken` so the UI can clear
  it. Managed from the desktop per-bot Skills/Workflows windows.
- **Agent config isolation** (`config.DriverEnvWith`): each bot's `claude` runs
  with `CLAUDE_CONFIG_DIR=~/.xclaw/<id>/.claude` so it does NOT inherit the
  operator's `~/.claude` (user-scope skills + installed plugins would otherwise
  leak into every bot). Auth is env-based (`ANTHROPIC_*`), so this is safe; CLI
  built-in skills still load, and the per-bot catalog comes from the sandbox.
  Opt out with `agent.inheritUserConfig: true` (trusted single-operator only).
- **ClaudeDriver headless invariants** (`core/agent/claude.go`): always spawns
  `claude -p - --output-format stream-json --verbose --permission-mode bypassPermissions`,
  feeding the prompt on **stdin** (`-p -`, not an argv element, so a large prompt
  can't hit ARG_MAX — and never inherits the daemon's fd 0, which carries the
  control-bus cap token; MLT-40). Bypass is mandatory — there is no terminal to
  answer approval prompts, so any other mode hangs the turn; it also grants every
  tool, so no `--allowedTools` is passed (claude 2.1+ rejects `*` in allow rules).
  Output is **plain stream-json** (one event per complete content block) — the
  driver does NOT request `--include-partial-messages`, so there is no token-level
  delta path or dedup. `--append-system-prompt` is re-sent every turn including
  resumes: it does NOT persist across `--resume`, so skipping it would drop the
  non-overridable `SecurityPrefix` + SOUL (its tokens are a prompt-cache hit
  anyway). The `result` line populates `TokenUsage` with cached-input tokens
  (`cache_read_input_tokens`) and `CostUSD` (`total_cost_usd`). Upstream
  rate-limit / overload / usage-cap conditions (HTTP 429/503/529, "usage limit
  reached", …) are classified as `AgentEvent.Transient` (`core/agent/classify.go`)
  with a reset-window `RetryHint`; the gateway replies `busyReply` ("服务繁忙") for
  a transient terminal error instead of the generic errorReply. Tool/permission
  policy is intentionally NOT in `agent.Request`; it is a fixed claude-only invariant.
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
  `agent` (`cron`, `toolProgress`, `inheritUserConfig`); the group-gating lists
  (`mentionFreeGroups`, `knownBotUids`, `allowedBotUids`, `botBlocklist`) plus
  `groupConfigDir` and `onBehalfOf` are top-level defaults a bot may override — a
  per-bot value REPLACES the default. (Skills/workflows are **not** config fields;
  they're installed per-bot on the filesystem — see the marketplace bullet above.)
  `core/config.example.json` is the canonical field list.
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
  `configstore` (read/write `~/.xclaw/config.json` + per-bot SOUL/AGENTS),
  `assetlib` (the shared per-bot install/uninstall/prune symlink primitives),
  `skills` (marketplace catalog CRUD over `~/.xclaw/skills/` bundles **plus**
  per-bot `Bot*`/`Install`/`Uninstall` over `~/.xclaw/<id>/skills`) and
  `workflows` (same, for `*.js`), all with slug + path-traversal validation,
  `octocli` (bundle/install/upgrade the octo-cli
  companion), `secrets` (tokens in the OS credential store via go-keyring,
  zero cgo; injected at runtime, **never** written to config.json).
- **Frontend** (`frontend/src`): `lib/store.svelte.ts` is the single reducer —
  it folds `xclaw:event` envelopes into bots/sessions/messages and owns the
  rAF typewriter/coalescing. Components in `lib/components/` (Rail · Conversations
  · Transcript · Bubble · Composer · ConfigEditor · SkillsPanel · WorkflowsPanel ·
  WorkspacePanel · FilePreview · TokenUsage · Avatar); tokens in `lib/styles/theme.css`.
- **Workspace sidebar** (`WorkspacePanel`): a chat-header toggle button opens an
  inline (NOT modal — no scrim) right-hand column inside `.content`, showing the
  selected session's sandbox file tree. Selecting a file opens `FilePreview` as a
  wide pane that takes the chat's flex slot (rail · preview · tree) — App owns the
  `previewPath` state; ✕/Esc clears it back to the chat, and switching session
  clears it too. `FilePreview` renders by kind — Markdown (rendered, with a
  Rendered/Raw toggle), code/text (line-number gutter + `highlight()` from
  `lib/markdown.ts`), images (Fit/Actual), and PDF (inline `<iframe>` data-URL) —
  reusing the chat's markdown CSS + `onMarkdownCopyClick`. Backend reads the
  filesystem directly via two Wails methods (`WorkspaceTree`/`WorkspaceFile` →
  `desktop/internal/workspace`); since the frontend doesn't carry the channel kind,
  the package resolves the cwd by trying both `dm`/`group` hashes
  (`sandbox.SessionDirName`) and using whichever dir exists. Read-only + sandboxed:
  never follows symlinks, skips `.claude/`, caps depth/entries/file-size (1 MiB for
  text, 8 MiB for base64 images/PDFs).
- **Edit Bots / Manage Skills / Manage Workflows / Token Usage are sibling modals**
  (`ConfigEditor` · `SkillsPanel` · `WorkflowsPanel` · `TokenUsage`), all opened over
  the console from the rail gear menu (and tray) via `xclaw:open-editor` / `-skills` /
  `-workflows` / `-usage` events — same scrim + centered card + header/✕ chrome (keep
  them visually consistent). SkillsPanel/WorkflowsPanel are **per-bot**: a bot
  picker in the header, a "本 Bot 技能/工作流" section (own + installed assets, with
  install/uninstall and an own-content editor; a dangling install shows a 失效
  badge and is uninstall-only) and a marketplace section with install/已安装
  toggles — install/uninstall take effect on the next turn, no RestartCore.
  ConfigEditor no longer has any skill/workflow checklist. TokenUsage is read-only:
  per-bot cumulative input/output/cached
  tokens + cost + turns, plus an all-bots total, from the privileged `usage.stats`
  control command (backed by core/store's `token_usage` table, accumulated each
  turn_done in the gateway; persists across restarts). NOTE: `window.confirm/alert` are
  no-ops in the Wails webview — use an in-app dialog for any confirmation.
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
