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
// session.reset). The peer-credential gate (MLT-29) already blocks any cross-uid
// process; this token draws the boundary the peer-cred check cannot — the
// operator's GUI (which holds the token) vs. the spawned agent's CLI, which runs
// as the same uid as the daemon.
//
// Read-only/health commands (health, bots.list, session.history, cron.list) stay
// open: they leak nothing about other sessions the agent's own bot can't see, and
// the cross-session event stream is gated separately in Server.Broadcast.
var privilegedControlCommands = []string{
	"session.send",
	"session.reset",
	"secret.inject",
	"cron.create",
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
