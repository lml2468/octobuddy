# Platform notes

What's tested where, the per-platform dependencies, and the gotchas we've
hit. Each section is a survival guide for that platform — read before
shipping a change that might break it.

## macOS — first-class

The primary target. Every release ships a signed + notarized `.app`.

**Tested on:** macOS 13+ (deployment target in `Info.plist` is 13.0.0).

**Dependencies:** none beyond a stock install. Webview is system WKWebView.
Daemon is zero-cgo.

**Build:**
```sh
zsh scripts/package-desktop.sh         # ad-hoc signed
OCTOBUDDY_SIGN_IDENTITY="Apple Development: …" zsh scripts/package-desktop.sh   # Dev-ID signed
zsh scripts/release.sh                 # signed + notarized + stapled
```

**Signing + notarization:** see `scripts/release.sh` header. One-time setup:
1. Developer ID Application cert in login Keychain
2. `xcrun notarytool store-credentials octobuddy-notary --key …` for an
   App Store Connect API key

**Gotchas:**
- Traffic-light overlap on the leftmost UI pane. Only the bot rail clears
  them (taller rail header); list/chat headers stay compact. See CLAUDE.md
  "macOS gotchas".
- Frameless + transparent window: corners outside CSS `border-radius: 4px`
  show through the OS to give a subtle rounding. Custom traffic lights
  live in-app, not native.
- Keychain prompts on an unsigned/re-signed binary: allow once; a stable
  signing identity makes the grant stick across rebuilds.
- After changing Go binding signatures: `cd desktop && wails3 generate
  bindings -d frontend/bindings`.
- Tray icon: programmatically-generated template PNG (`xMarkTemplatePNG`
  in `desktop/trayicon.go`), so it adapts to light/dark menu bar without
  shipping multiple asset files.
- macOS resolves `/var/folders/…` (TMPDIR) through a `/private/var` symlink.
  Anything that uses `O_NOFOLLOW` on the leaf is fine, but if you ever
  add `O_NOFOLLOW` walks on parents, account for this firmlink chain.

## Linux — supported

GUI tested on Ubuntu 24.04; daemon runs anywhere Go runs.

**Dependencies (GUI):**
- GTK4 (`libgtk-4-dev`)
- WebKitGTK 6.0 (`libwebkit2gtk-6.0-dev`)
- `pkg-config`

```sh
sudo apt install libgtk-4-dev libwebkit2gtk-6.0-dev pkg-config
```

**Dependencies (daemon-only / headless):** none. The pure-Go SQLite driver
and zero-cgo build mean the daemon binary is fully static.

**Build:**
```sh
cd desktop && wails3 task package     # native GUI build
# OR daemon-only (no GUI deps required):
cd core && CGO_ENABLED=0 go build -o octobuddy-daemon ./cmd/octobuddy-daemon
```

**Distribution:** raw binary today. AppImage/deb/flatpak NOT shipped (on
the roadmap — see PR-? in audit). Run `chmod +x ./octobuddy && ./octobuddy`.

**Gotchas:**
- Webview rendering varies by WebKitGTK version. Newer is better; 6.0+ is
  the floor.
- OS keychain backend: libsecret (D-Bus). On headless systems without a
  secret service, `go-keyring` falls back gracefully to the file backend
  under `~/.octobuddy/<id>/secrets/`.
- Autostart NOT implemented (the "Launch at Login" tray row is hidden on
  Linux/Windows via `autostart.Supported()` returning false).
- Single-instance lock is filesystem-based via `application.SingleInstance`;
  if you see "OctoBuddy is already running" but no window, check for stale
  locks under `~/.config/`.

## Windows — supported

GUI tested on Windows 11.

**Dependencies (GUI):**
- WebView2 runtime (preinstalled on Win10 19041+ / Win11). Older Win10
  needs the WebView2 Evergreen installer.

**Dependencies (daemon-only):** none.

**Build:**
```sh
# On a Windows host (cross-compile of the GUI is not supported by Wails):
cd desktop && wails3 task package
# Daemon cross-builds from any OS:
cd core && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o octobuddy-daemon.exe ./cmd/octobuddy-daemon
```

**Distribution:** `.exe` today (UNSIGNED — SmartScreen will warn first-time
users; on the roadmap to fix). Place `octobuddy-daemon.exe` beside
`octobuddy.exe`.

**Gotchas:**
- Webview is Microsoft Edge WebView2 — Chromium under the hood, same as
  Chrome devtools. Behaves nearly identical to macOS/Linux WebKit for
  CSS/JS; one known divergence is font fallback (Geist Mono renders
  slightly differently for CJK glyphs).
- Path separators: always use `filepath.Join`, never raw `/`. Verified by
  CI (grep gate on `core/`).
- Shell out via `exec.Command("cmd", "/C", "start", "", <path>)` — the
  empty title argument is load-bearing (`start` interprets the first
  quoted arg as window title, not file path). See
  `desktop/main.go::openLogInConsole`.
- `-race` is disabled on the Windows CI job: the runner ships no C compiler
  and our daemon is `CGO_ENABLED=0`. Lint + build + non-race tests still run.
- Authenticode signing is NOT wired into CI yet; an unsigned `.exe`
  triggers Windows SmartScreen warnings on first launch. Workaround for
  CI/testing: right-click `Properties → Unblock`.
- OS keychain backend: Windows Credential Manager (via `go-keyring`).

## Cross-platform discipline

- Platform-specific code lives in `*_darwin.go` / `*_linux.go` /
  `*_windows.go` files with build tags, never inline `runtime.GOOS == ...`
  in business code. See `core/safepath/safe_*.go` and
  `core/cmd/octobuddy-daemon/peercred_*.go`.
- Use `filepath` (not `path`) for OS paths; `path` is for `/`-separated
  URL/JSON keys only.
- Shell calls go through `exec.Command(cmd, args...)` with a slice — never
  via a single string. Verified across `core/` and `desktop/`.
- The CI matrix gates `go build` + `go test` on all three platforms;
  desktop `wails3 task package` runs on macOS + Ubuntu + Windows runners.
  A change that's "only tested on my Mac" will be caught.

## Known open gaps (to fix)

These are scheduled per the audit roadmap; PRs welcome:

- Windows Authenticode signing in CI (PR-E)
- Linux AppImage packaging step (PR-E)
- Auto-update (sparkle / wails-updater equivalent) on all three platforms (PR-G)
- DPI / high-density display explicit handling and verification (no known
  bugs today, but no test coverage either)
