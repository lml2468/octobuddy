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

### Changed
- Formatted the entire `core/` tree with `gofmt`.
- Converted mixed-language code comments to English.

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

<!--
Going forward, summarize notable changes here under Added / Changed / Deprecated
/ Removed / Fixed / Security, and cut a versioned section on each tagged release.
-->
