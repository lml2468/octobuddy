<!-- Thanks for contributing to XClaw! Please fill out the sections below. -->

## What & why

<!-- What does this change do, and why is it needed? Link any related issue: Closes #123 -->

## Which piece(s)

- [ ] `core/` (Go daemon `xclawd`)
- [ ] `app/` (Swift macOS app)
- [ ] `proto/` (control-bus contract)
- [ ] docs / CI / tooling

## Checklist

- [ ] `cd core && gofmt -l .` reports nothing, and `go vet ./...` passes
- [ ] `cd core && go test -race ./...` passes
- [ ] Swift changes: `cd app && swift build && swift test` pass
- [ ] Touched the control-bus contract? Updated `proto/README.md` **and** both sides (core + app)
- [ ] Touched the gateway/router/store? Preserved the documented invariants in `CLAUDE.md` (sessionKey derivation, per-session lock ordering, group-context delta-before-cache)
- [ ] New code carries comments in English

## Notes for reviewers

<!-- Anything reviewers should pay special attention to (security, concurrency, protocol compat). -->
