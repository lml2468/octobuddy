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
- Conversation history survives restarts: on connect the GUI hydrates each bot's
  transcript from the gateway's persisted store via the existing `session.history`
  control command (matched back by command id), instead of keeping its own
  store. No duplicate persistence; agent-agnostic (no coupling to a driver's
  on-disk session files).
- Edit Bots editor overhaul: edit the **model** and a bot's **persona/behavior**
  (SOUL.md / AGENTS.md) from the GUI; inline URL validation, reveal toggles on
  token fields, section icons, a larger window, and an apiURL subtitle in the
  bot list. Saving now **merges** into `config.json` so keys the editor doesn't
  manage (rateLimit, context, top-level agent defaults) are preserved instead of
  dropped — and `agent.model` is no longer lost on save. Form fields render
  label-on-top with full-width, left-aligned values (`.labelsHidden()` + an
  in-field `prompt:`) so long URLs/tokens read cleanly, instead of the grouped
  `Form` default that pulled each field's title into a leading label and crammed
  the value against the trailing edge. The editor now lives in its **own
  resizable window** (opened via ⌘, or the menu-bar "Edit Bots…") instead of a
  `Settings` scene: a master/detail `NavigationSplitView` needs a split-view
  window to render a flush, full-height sidebar — inside a Settings pane it
  collapsed to a floating inset card with a dead top gap.

### Fixed
- Control-bus server no longer crashes the daemon when a client disconnects
  during a broadcast. `client.enqueue` used `select { case sendCh <- … : default }`,
  but a *closed* channel send is never caught by `default` and panics; a GUI
  disconnecting mid-turn (now common, with restarts) could take down `xclawd`.
  The write loop now stops via a separate `done` channel and `sendCh` is never
  closed, so producers can never send on a closed channel. Regression test added.
- Control handlers consolidated: single-bot and multi-bot modes now share one
  command dispatcher, so `bots.list` (previously missing in single-bot mode,
  breaking a GUI client's roster/history bootstrap) and every other command
  behave identically. `session.send` now runs its turn under the daemon's
  cancellable context (shutdown aborts the in-flight `claude` subprocess instead
  of orphaning it) and reports a router drop / turn error back as an `error`
  event instead of swallowing it with `_, _ =`, so the GUI no longer hangs.
- `proto/README.md` reconciled with the implementation: corrected the
  `session.send` body (`{uid}`, not `{channel}`), documented the real events
  (`session.reply`, `session.activity` kinds incl. `thinking`) and the
  `session.reset` command, marked `bot.start`/`bot.stop`/`config.reload` as
  planned, and removed the never-emitted `rateLimit.hit`/`cron.fired` events.
- Octo connector: `botUID` is now guarded by the connector mutex (it was written
  by `Run`'s re-registration path while the sink callbacks / a concurrent turn
  read it — a data race that could corrupt self-message filtering around a
  reconnect).
- Octo socket: the decrypt-failure strike map no longer wipes all counts when it
  hits its cap (which zeroed an in-flight poison message's count so it never
  reached the ack-and-drop threshold → infinite redelivery). It now evicts only
  other entries, bounded, preserving the current message's count.
- Octo wire: `writeString` clamps an over-long field (>65535 bytes) on a rune
  boundary so the uint16 length prefix can never desync from the payload and
  corrupt the rest of a frame.
- Gateway logs (instead of silently swallowing) `store.Resume`/`SaveResume`
  errors, so a transient DB failure that drops conversation continuity is
  diagnosable.
- `ClaudeDriver.Query` reader goroutines now send events via a `select` on
  `ctx.Done()`, so a cancelled/abandoned consumer can't wedge a reader on a full
  channel (leaking the goroutine + the `claude` subprocess).
- `truncateParams` truncates tool-input previews on a rune boundary (no more
  invalid UTF-8 in `session.tool` events).
- Documented accepted security tradeoffs in code: WuKongIM AES-128-CBC is
  protocol-dictated and unauthenticated (inbound IM text is treated as untrusted
  and fenced regardless); SSRF URL validation is literal-IP-only for
  operator-trusted config (no DNS-rebind protection); the gateway token reaches
  the agent via the child environment (per SECURITY.md). `http` URLs now accept
  any loopback form (e.g. `127.0.0.2`, `::1`), not just the `127.0.0.1` literal.
- "Save & Restart" (and "Restart Core") in the macOS app now reliably reconnects.
  The restart stopped the old daemon and started a new one without waiting for
  the old one to exit; because the daemon removes its control socket on shutdown
  (`defer os.Remove`), the dying daemon's cleanup could delete the *new* daemon's
  socket file, leaving the GUI unable to connect (bot stuck disconnected).
  `CoreSupervisor.stop()` now awaits actual process exit (SIGTERM, then SIGKILL
  after 3s) and all restart paths fully stop the current daemon before launching
  the next, so the two never overlap on the same socket path.
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
