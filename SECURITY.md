# Security Policy

XClaw drives coding agents with broad tool access and connects to IM networks,
so it sits in a security-sensitive position. We take reports seriously.

## Reporting a vulnerability

**Please do not open a public issue for security vulnerabilities.**

Report privately via GitHub's [private vulnerability reporting][gh-report]
("Report a vulnerability" on the repository's **Security** tab). Include:

- a description of the issue and its impact,
- steps to reproduce or a proof of concept,
- affected component (`core`, `app`, `proto`) and version/commit.

We aim to acknowledge reports within a few days and will keep you updated on
remediation. Please give us reasonable time to fix the issue before any public
disclosure.

[gh-report]: https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability

## Supported versions

XClaw is pre-1.0 and under active development. Only the latest `main` receives
security fixes until a tagged release line exists.

## Threat model & known limitations

XClaw's design defends against prompt injection from untrusted group chat
(`core/safety/`, `core/groupctx/`). However, the project is **pre-1.0 and not yet
hardened for hostile multi-tenant deployment.** Be aware of these *known,
documented* limitations before exposing a bot to untrusted users:

- **Agent runs with full tool access.** The Claude driver always spawns with
  `--permission-mode bypassPermissions` (a headless invariant — there is no
  terminal to answer approval prompts). The agent can run arbitrary tools,
  including Bash.
- **The session sandbox is not a security boundary.** `core/sandbox/` gives each
  session a *starting* working directory, not a chroot/jail. An agent with Bash
  can still reach absolute paths outside it. Isolation across spaces relies on
  running one bot per space with a separate `cwdBase`.
- **Tokens are currently stored in plaintext** in `~/.xclaw/config.json` and are
  injected into the agent subprocess environment. Keychain/secret-store
  integration is planned but not yet implemented. Protect that file accordingly.
- **IM wire crypto is constrained by protocol compatibility.** The Octo/WuKongIM
  connector (`core/im/octo/`) reproduces the upstream handshake byte-for-byte
  (curve25519 DH → MD5-derived AES-128-CBC, IV from the server salt). These
  primitives are weak by modern standards but are dictated by wire compatibility
  with the server, not a free design choice; the connector only *decrypts*
  server-sent frames.
- **SSRF validation is config-time.** `core/config/` validates configured URLs,
  but does not currently defend against DNS rebinding at request time.

If you find a way to defeat the prompt-injection defenses, escape the sandbox in
a way the docs claim is prevented, or leak secrets through events/logs, that is
in scope — please report it.
