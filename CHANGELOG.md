# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- `LICENSE` (MIT), `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, and
  this changelog — open-source baseline.
- GitHub Actions CI: Go build/vet/`gofmt`-gate/`test -race`, zero-cgo
  cross-compile matrix (darwin/linux/windows), and Swift build/test. Plus PR and
  issue templates.
- Runtime secret injection (`secret.inject` control command, previously specced
  but unimplemented): the core now holds bot tokens in an in-memory store and
  resolves them lazily (per REST request / per turn), so tokens can be injected
  at runtime instead of read from `config.json`. Tokens in the config file are
  now **optional** — a bot without one waits ("awaiting secret") until injected,
  then connects. This is the core-side half of moving secrets to the macOS
  Keychain.
- macOS app stores bot tokens (`octoToken`, `gatewayToken`) in the **Keychain**
  instead of plaintext `config.json`, and injects them to the core at runtime
  (`secret.inject`) after connecting. Plaintext tokens left in an existing
  `config.json` are migrated into the Keychain and stripped from the file on
  launch. `config.json` remains a supported plaintext fallback for headless
  deployments.

### Changed
- Formatted the entire `core/` tree with `gofmt`.
- Converted mixed-language code comments to English.
- macOS app UI redesigned to Tahoe / Liquid-Glass conventions: a unified window
  toolbar (status + Reset/Restart) with navigation title/subtitle, a sidebar
  `List` with status symbols and session-count badges, session cards with depth
  (material/border/shadow) and a clear type hierarchy, a chat-style composer
  (material bar + circular accent send button), material info banners, and a
  proper menu-bar popover (`.menuBarExtraStyle(.window)`) with hoverable actions.
- macOS app console is now a **chat transcript**: each turn renders as bubbles
  (user trailing/accent, assistant leading/surface, tool calls as centered
  chips) instead of per-session status cards, with the bot name + connection
  status moved into the window title bar and auto-scroll to the latest message.
  `AppState` gained a per-session message history; the GUI echoes its own sent
  messages locally (the bus doesn't return them). A `#if DEBUG`/env preview seam
  (`XCLAW_UI_PREVIEW`) renders mock data for screenshots without a daemon.
- Chat polish: assistant bubbles hug their content (short replies are small
  bubbles, not full-width boxes), and an animated "typing…" indicator shows
  between sending a message and the agent's first output. Verified in light and
  dark mode via on-device screenshots.
- Chat affordances: each bubble reveals a timestamp + copy button on hover; when
  a bot has more than one session, a segmented session picker switches between
  them (single session renders directly).
- App icon: a generated graphite/chat-bubbles `AppIcon.icns` (reproducible via
  `app/Packaging/make-appicon.sh`), embedded by the packager and referenced from
  the bundle's `Info.plist`.

### Fixed
- The router's per-session lock map and per-user/per-session rate-limit buckets
  no longer grow without bound: a new `Router.Reap` evicts idle entries (lock
  eviction is refcount-guarded so an in-flight turn is never reaped). A bot now
  runs a periodic reaper that also enforces the session/sandbox TTLs over the
  daemon's lifetime, instead of sweeping only once at startup. The reaper stops
  on context cancellation.
- Octo connector shutdown & cancellation: `socketConn.run` now closes the
  connection on context cancellation so the blocking WebSocket read unblocks and
  the daemon shuts down promptly (gorilla's `ReadMessage` does not observe
  context); `connectOnce` always releases the socket on return, so reconnects no
  longer accumulate connections/goroutines. Inbound turns and outbound REST
  calls now use the connector's run context instead of `context.Background()`,
  and previously-swallowed errors (gateway turn, send-typing, send-reply) are
  now logged.
- `ClaudeDriver.Query` no longer risks a send-on-closed-channel panic: the
  stdout and stderr readers are now joined with a `WaitGroup` so the event
  channel is closed exactly once, after both finish and after `cmd.Wait`
  (previously the stdout reader closed the channel while stderr could still be
  sending, and `Wait` could run before stderr was drained). Adds a `WaitDelay`
  so a lingering grandchild can't hang the turn after context cancellation.
- macOS app resilience & idioms: `CoreSupervisor` now trips a circuit breaker
  (reports `.failed` after repeated immediate crashes) instead of restarting a
  broken daemon forever; `ControlClient` drops a force-unwrap, logs malformed
  envelopes/frame errors via unified `os.Logger` instead of dropping them
  silently; previously-swallowed app send/migration errors are now logged.
  Status indicators gained VoiceOver labels. The unnecessary
  `disable-library-validation` entitlement was removed (the app exec's the
  daemon as a subprocess, never loading it in-process).
- macOS app UI modernized to current SwiftUI idioms: `ContentUnavailableView`
  empty states, `@Environment(\.openWindow)` to surface the console (replacing a
  title-string window lookup), `LazyVStack` for the session list, `@FocusState`
  on the composer, and a stable `.id` on the bot form for clean switching.
  VoiceOver reads bot rows and session cards as combined elements; the `AppState`
  reducer's `bot.status` / `error` / non-event paths are now directly tested.
- macOS app concurrency made compiler-checked: `CoreSupervisor` is now an
  `actor` (its non-`Sendable` `Process` state is actor-isolated; restart timing
  uses a cancellable `Task`), and `ControlClient` is a checked `Sendable` class
  (its `fd`/id counter live behind an `OSAllocatedUnfairLock`, the `LineFramer`
  is local to the read loop). No production app/core type uses
  `@unchecked Sendable` anymore.
- macOS app separation of concerns: bot-configuration editing (load/add/remove/
  save, Keychain persistence, legacy-token migration) extracted from `AppModel`
  into a dedicated `@Observable ConfigEditorModel`, so `AppModel` now owns only
  runtime lifecycle, bus connection, and messaging.
- The embedded `xclawd` no longer leaks when the app dies non-gracefully
  (crash/force-quit): a new `-exit-with-parent` flag (passed by the app) makes
  the daemon shut down when it's orphaned (reparented to pid 1), freeing the
  control socket. Found via a real packaged-app smoke test; graceful Quit was
  already handled by `CoreSupervisor.stop()`.

<!--
Going forward, summarize notable changes here under Added / Changed / Deprecated
/ Removed / Fixed / Security, and cut a versioned section on each tagged release.
-->
