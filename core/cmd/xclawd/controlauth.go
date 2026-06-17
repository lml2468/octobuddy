package main

import (
	"bufio"
	"io"
	"os"
	"strings"

	"github.com/lml2468/xclaw/core/control"
)

// privilegedControlCommands are the operator-only control-bus commands gated
// behind the GUI capability token (MLT-37). Each is a GUI→daemon operation with
// no sanctioned agent path — scheduling prompts that fire as the owner
// (cron.*, a prompt-injection-into-future-turns persistence primitive), pushing
// secrets (secret.inject), injecting/clearing sessions (session.send,
// session.reset), and reading stored data across sessions/bots (session.history,
// cron.list). The peer-credential gate (MLT-29) already blocks any cross-uid
// process; this token draws the boundary the peer-cred check cannot — the
// operator's GUI (which holds the token) vs. the spawned agent's CLI, which runs
// as the same uid as the daemon.
//
// session.history and cron.list are privileged because their handlers take an
// attacker-controllable botId + sessionKey (session.history) / botId (cron.list)
// with NO scoping check, so a prompt-injected same-uid agent could read any
// session's stored plaintext history or enumerate the owner's scheduled prompts
// across any bot — the at-rest twin of the cross-session event stream that is
// gated in Server.Broadcast. sessions.list is privileged for the same reason: it
// enumerates EVERY persisted session for a bot with a message preview, an even
// broader cross-session disclosure than a single session.history read.
// sessionKeys are low-entropy (DM = uid, group = channelId) and an injected agent
// already sees peer uids / channel ids, so targeting is trivial; leaving these
// open defeats the cross-session boundary this gate establishes. The GUI is the
// only sanctioned caller and it authenticates before issuing them (the auth send
// precedes all other sends on its FIFO connection), so gating does not break it.
//
// Open commands (health, bots.list) stay open: low-value daemon/roster metadata
// the agent's own config already implies, with no cross-session disclosure.
var privilegedControlCommands = []string{
	"session.send",
	"session.reset",
	"secret.inject",
	"session.history",
	"sessions.list",
	"cron.create",
	"cron.list",
	"cron.delete",
}

// maxTokenBytes caps the capability-token read so a misbehaving launcher can't
// stream unboundedly into daemon memory. A hex/base64 token is well under this.
const maxTokenBytes = 4096

// configureBusAuth arms the control server's capability-token gate. The token is
// delivered out-of-band over the daemon's stdin (the launcher — the GUI — writes
// it to a pipe it owns), so the secret never appears in an env var or argv (both
// world-readable via /proc/<pid>/ on Linux) and the spawned agent never sees it:
// the daemon launches the agent CLI with a fresh stdin pipe of its own, so the
// agent does not inherit fd 0. The token is read once at startup into memory.
//
// Fail closed:
//   - authStdin false (bare CLI/dev, no launcher token): the gate arms with an
//     empty token, so no connection can authenticate and every privileged command
//     is denied. Read-only commands still work.
//   - authStdin true but the read fails or yields an empty token: abort. A
//     launcher that asked for the gate must never silently fall back to open.
func configureBusAuth(srv *control.Server, authStdin bool) {
	token := ""
	if authStdin {
		t, err := readControlToken(os.Stdin)
		if err != nil {
			fatal("control auth: read token from stdin: %v", err)
		}
		if t == "" {
			fatal("control auth: empty token on stdin")
		}
		token = t
	}
	// Never log the token.
	srv.SetAuth(token, privilegedControlCommands)
}

// readControlToken reads the first line (the capability token) from r, bounded by
// maxTokenBytes, and trims surrounding whitespace. A trailing newline is optional
// — a writer that closes the pipe without one (EOF) is accepted.
func readControlToken(r io.Reader) (string, error) {
	br := bufio.NewReader(io.LimitReader(r, maxTokenBytes))
	line, err := br.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
