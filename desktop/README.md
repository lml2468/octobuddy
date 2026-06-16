# XClaw desktop (`desktop/`)

The XClaw desktop app — a **Wails v3** (Go backend) + **Svelte 5 / TypeScript**
frontend. It is a thin **control-bus client**: it never talks to Claude or the IM
directly. It spawns and supervises the `xclawd` daemon (`../core`), dials its
control socket, and folds the daemon's event stream into a clean WeChat/iMessage-grade
chat UI. Swapping the GUI never touches `core/`.

Module: `github.com/lml2468/xclaw/desktop`. It pulls `core` in via a local
`replace` in the repo's `go.work`.

## Develop

Needs the Wails v3 CLI:

```bash
go install github.com/wailsapp/wails/v3/cmd/wails3@latest
```

```bash
# From the repo root — builds core + runs `wails3 dev` (needs ~/.xclaw/config.json):
zsh scripts/run-dev.sh
zsh scripts/run-dev.sh --seed-config     # write a starter config first
zsh scripts/run-dev.sh --preview         # UI preview: mock data, no daemon

# Frontend build + typecheck
cd desktop/frontend && npm run build && npx svelte-check

# After changing Go binding signatures, regenerate the TS bindings:
cd desktop && wails3 generate bindings -ts -d frontend/bindings
```

**UI preview mode** (`XCLAW_PREVIEW=1`, with `XCLAW_PREVIEW_THEME=dark|light`,
`XCLAW_PREVIEW_EMPTY=1`, `?editor=1` / `?skills=1`) seeds mock data and skips the
daemon, so the UI can be screenshotted and geometry-asserted in headless Chrome
without a live bot.

## Layout

```
main.go            app + frameless window + system tray + single-instance
xclawservice.go    Wails-bound bridge: spawn xclawd, dial UDS, forward
                   xclaw:event, expose command/config/skills methods
internal/
  control          UDS/NDJSON client over core/control/wire
  core             supervisor: resolve binary → spawn → stop/restart
  configstore      ~/.xclaw/config.json + per-bot SOUL/AGENTS + skill allow-list
  skills           CRUD over the ~/.xclaw/skills/ catalog bundles
  octocli          bundle/install/upgrade the octo-cli companion
  secrets          tokens in the OS credential store (go-keyring, zero cgo)
frontend/src
  lib/store.svelte.ts    single reducer: folds xclaw:event into the view model
  lib/components/        Rail · Conversations · Transcript · Bubble · Composer ·
                         ConfigEditor · SkillsPanel · Avatar
  lib/styles/theme.css   design tokens
```

## Packaging

`../scripts/package-desktop.sh` cross-compiles `xclawd` (mac universal + win/linux),
fetches + bundles the latest `octo-cli`, embeds both in `Contents/Helpers/`, and
signs inside-out (ad-hoc by default; Developer-signs + notarizes when
`XCLAW_SIGN_IDENTITY` / `XCLAW_NOTARY_PROFILE` are set).

See [`../CLAUDE.md`](../CLAUDE.md) for the committed design direction and the
macOS gotchas (traffic-light clearance, keychain injection, `window.confirm`
being a no-op in the webview).
