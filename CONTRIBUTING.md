# Contributing to XClaw

Thanks for your interest in improving XClaw! This guide covers how to build,
test, and submit changes. By contributing you agree your work is licensed under
the project's [MIT License](LICENSE).

## Repository shape

XClaw is a monorepo of three pieces that version together against one contract
(see the root [`README.md`](README.md) and [`CLAUDE.md`](CLAUDE.md) for the full
architecture):

- `core/` — Go daemon `xclawd` (the agent gateway). Single static binary, zero cgo.
- `app/` — Swift macOS app (SwiftPM, macOS 14+), a control-bus client.
- `proto/` — the language-neutral control-bus contract shared by core and app.

## Prerequisites

- **Go** matching `core/go.mod` (currently Go 1.26).
- **Swift / Xcode** for the macOS app (macOS 14+), only if you touch `app/`.
- Tests need **no API key** — they run against recorded fixtures and live CLI
  spawns that only assert parsing/wiring.

## Build & test

```bash
# Go core (run from core/)
cd core
go build ./...
go vet ./...
gofmt -l .                 # must print nothing
go test -race ./...        # the race detector is part of the bar

# Single package / single test
go test ./gateway/ -run TestName

# Swift app (run from app/)
cd app && swift build && swift test
```

All four — `gofmt -l` clean, `go vet`, `go build`, `go test -race` — must pass
before a PR can merge. CI enforces them (`.github/workflows/ci.yml`).

## Coding standards

- **English only** in code, comments, and commit messages. CJK string *fixtures*
  in tests (verifying UTF-8 / non-ASCII handling) are fine; prose is not.
- Run `gofmt -w .` before committing. Prefer `goimports` ordering.
- Follow the existing package-doc style: each package starts with a doc comment;
  ports from the upstream `cc-channel` TypeScript name their source file.
- **Don't break the abstraction.** Everything downstream of `agent.Driver`
  depends only on the `agent.AgentEvent` vocabulary — never reach into
  driver-specific behavior from the gateway/router/store/control bus. Adding a
  new agent means writing one new `Driver`, nothing else.
- Preserve the documented invariants in `CLAUDE.md` when touching the gateway:
  sessionKey derivation (never fall back to `""`), the per-session lock held
  across the whole turn, and the group-context "build the delta **before**
  caching the current message" ordering.

## Commit messages

Use [Conventional Commits](https://www.conventionalcommits.org/):
`feat(scope): …`, `fix(scope): …`, `refactor(scope): …`, `docs: …`, `test: …`,
`chore: …`. Scope is usually a package (`gateway`, `router`, `agent`, `config`,
`app`). Keep the subject in the imperative mood.

## Pull requests

1. Branch off `main`.
2. Keep PRs focused; one logical change per PR.
3. Fill out the PR template checklist.
4. If you touch the control-bus contract, update `proto/README.md` **and** both
   sides (core + app) in the same PR — they version in lockstep.
5. Add or update tests for behavior changes. Security- and protocol-critical
   code (crypto, wire parsing, prompt-safety, SSRF validation) should get
   adversarial / error-path tests, not just happy-path.

## Reporting security issues

Do **not** open a public issue for vulnerabilities. See [`SECURITY.md`](SECURITY.md).
