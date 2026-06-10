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

<!--
Going forward, summarize notable changes here under Added / Changed / Deprecated
/ Removed / Fixed / Security, and cut a versioned section on each tagged release.
-->
