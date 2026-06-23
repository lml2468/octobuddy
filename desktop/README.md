# OctoBuddy desktop (`desktop/`)

The OctoBuddy desktop app — a **Wails v3** (Go backend) + **Svelte 5 / TypeScript**
frontend. It is a thin **control-bus client**: it never talks to Claude or the IM
directly. It spawns and supervises the `octobuddy-daemon` daemon (`../core`), dials its
control socket, and folds the daemon's event stream into a clean WeChat/iMessage-grade
chat UI. Swapping the GUI never touches `core/`.

Module: `github.com/lml2468/octobuddy/desktop`. It pulls `core` in via a local
`replace` in the repo's `go.work`.

## Develop

Needs the Wails v3 CLI:

```bash
go install github.com/wailsapp/wails/v3/cmd/wails3@latest
```

```bash
# From the repo root — builds core + runs `wails3 dev` (needs ~/.octobuddy/config.json):
zsh scripts/run-dev.sh
zsh scripts/run-dev.sh --seed-config     # write a starter config first
zsh scripts/run-dev.sh --preview         # UI preview: mock data, no daemon

# Frontend build + typecheck
cd desktop/frontend && npm run build && npx svelte-check

# After changing Go binding signatures, regenerate the TS bindings:
cd desktop && wails3 generate bindings -ts -d frontend/bindings
```

**UI preview mode** (`OCTOBUDDY_PREVIEW=1`, with `OCTOBUDDY_PREVIEW_THEME=dark|light`,
`OCTOBUDDY_PREVIEW_EMPTY=1`) seeds mock data and skips the daemon, so the UI
can be screenshotted and geometry-asserted in headless Chrome without a live
bot. Append `?settings=<tab>` (basic / octo / skills / workflows / schedules)
to land directly on a Settings-modal pane; `?memory` / `?files` to open the
workspace sidebar; `?usage` to open the token-usage modal.

## Layout

```
main.go            app + frameless window + system tray + single-instance
octobuddyservice.go    Wails-bound bridge: spawn octobuddy-daemon, dial UDS, forward
                   octobuddy:event, expose command/config/skills/workflows methods
internal/
  control          UDS/NDJSON client over core/control/wire
  core             supervisor: resolve binary → spawn → stop/restart
  configstore      ~/.octobuddy/config.json + per-bot SOUL/AGENTS
  skills           per-bot CRUD over ~/.octobuddy/<id>/.claude/skills/ bundles
  workflows        per-bot CRUD over ~/.octobuddy/<id>/.claude/workflows/*.js
  workspace        read-only sandboxed tree+file view of each session's cwd
  octoapi          REST helpers against the bot's Octo gateway
  octocli          bundle/install/upgrade the octo-cli companion
  autostart        macOS launch-at-login (com.mlt.octobuddy.desktop)
  secrets          tokens in the OS credential store (go-keyring, zero cgo)
  logfile          rotating ~/.octobuddy/logs/octobuddy.log
  safehttp         IM-scoped HTTP client (rebinding-proof dial guard)
frontend/src
  lib/store.svelte.ts    single reducer: folds octobuddy:event into the view model
  lib/components/        Sidebar · Transcript · Bubble · Composer · SettingsModal +
                         four panes (BasicInfoPane · OctoIntegrationPane · SkillsPane ·
                         WorkflowsPane) · SchedulesPane · TokenUsage · WorkspacePanel ·
                         FilePreview · Confirm · ErrorFooter · Avatar
  lib/confirm.svelte.ts  global confirm() — mounts <Confirm> programmatically
  lib/styles/theme.css   design tokens
```

## Packaging

`../scripts/package-desktop.sh` cross-compiles `octobuddy-daemon` (mac universal + win/linux),
fetches + bundles the latest `octo-cli`, embeds both in `Contents/Helpers/`, and
signs inside-out (ad-hoc by default; Developer-signs + notarizes when
`OCTOBUDDY_SIGN_IDENTITY` / `OCTOBUDDY_NOTARY_PROFILE` are set).

See [`../CLAUDE.md`](../CLAUDE.md) for the committed design direction and the
macOS gotchas (traffic-light clearance, keychain injection, `window.confirm`
being a no-op in the webview).
