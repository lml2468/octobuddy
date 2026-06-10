# XClaw macOS app

Native control plane / GUI for the `xclawd` Go core (`../core`). SwiftPM
package, macOS 14+, mirroring [Open Island](https://github.com/lml2468/open-vibe-island)'s
multi-target layout.

## Targets

- **XClawApp** — SwiftUI + AppKit shell. Owns `AppModel`, supervises the
  `xclawd` subprocess, renders bots / sessions from the control bus.
- **XClawCore** — control-bus client (NDJSON over Unix socket, see `../proto`),
  `AppState` reducer, models. Agent/IM-agnostic.

## Status

The app boots the core and renders the live event stream, with multi-bot
management:

- **CoreSupervisor** (`XClawCore`) spawns `xclawd` and restarts with backoff.
  Defaults to **multi-bot config mode** (`-config ~/.xclaw/config.json
  -control <sock>`); a single-bot flag mode remains for tests.
- **AppModel** (`XClawApp`) starts the core in config mode when
  `~/.xclaw/config.json` exists (else shows a `needs-config` state), owns the
  `ControlClient`, folds per-bot events into `AppState` (bucketed by botId),
  polls `bots.list`, and routes `send(botId)` / `reset` to the selected bot.
- **Console window** — a bot sidebar (per-bot connection status + session count)
  + the selected bot's session view + composer; **MenuBarExtra** shows the bot
  count and bus status.

Verified headlessly: integration tests spawn the REAL `xclawd -config` (both
directly and via CoreSupervisor in config mode), connect over the bus, and
assert `bots.list` returns the configured bots.

## Run (dev)

```bash
# from the repo root — builds the Go core, then launches the app which spawns it
# in multi-bot config mode (needs ~/.xclaw/config.json).
zsh scripts/run-dev.sh                 # launch (needs an existing ~/.xclaw config)
zsh scripts/run-dev.sh --seed-config   # write a starter ~/.xclaw config, then launch

# or directly (point the app at a prebuilt daemon)
cd core && go build -o .xclawd-dev ./cmd/xclawd
XCLAWD_BIN=$PWD/.xclawd-dev swift run --package-path ../app XClawApp
```

## Relationship to the core

The app never talks to Claude/Codex directly — it drives `xclawd`, which owns
all agent driving, session state, and IM connectivity. The app is one client of
the control bus; a web console or CLI could be others.
