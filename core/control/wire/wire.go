// Package wire defines the OctoBuddy control-bus contract: the NDJSON envelope, its
// codec, and the typed command/response/event bodies (see proto/README.md).
//
// It is a dependency-free leaf so any client — the daemon's own control server,
// the desktop GUI, a CLI — can depend on the wire vocabulary without pulling in
// agent/gateway internals. The daemon's `control` package re-exports these names
// for backward compatibility.
package wire

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
)

// ProtocolVersion is the envelope `v` field; clients must match (proto/README).
const ProtocolVersion = 1

// MaxFrameBytes caps a single NDJSON line so a peer that never sends a newline
// can't grow memory without bound.
const MaxFrameBytes = 4 * 1024 * 1024

// CmdAuth is the handshake command type a client sends to present its capability
// token (AuthBody). The daemon handles it internally — never the command handler
// — comparing the token in constant time and, on a match, marking the connection
// authorized for the privileged command set.
const CmdAuth = "auth"

// Kind discriminates the three envelope categories.
type Kind string

const (
	KindCommand  Kind = "command"  // client → server
	KindResponse Kind = "response" // server → client, correlated by id
	KindEvent    Kind = "event"    // server → client, unsolicited
)

// Envelope is the single wire unit. body is left raw so each side decodes it
// against the concrete command/event type named by `type`.
type Envelope struct {
	V    int             `json:"v"`
	Kind Kind            `json:"kind"`
	ID   string          `json:"id,omitempty"`   // correlates command↔response
	Type string          `json:"type"`           // e.g. "session.send", "session.text"
	TS   int64           `json:"ts,omitempty"`   // unix seconds
	Body json.RawMessage `json:"body,omitempty"` // type-specific payload
}

// ErrFrameTooLarge is returned when a line exceeds MaxFrameBytes.
var ErrFrameTooLarge = errors.New("control: frame exceeds max size")

// Encode marshals an envelope to a single NDJSON line (with trailing newline).
func Encode(e Envelope) ([]byte, error) {
	if e.V == 0 {
		e.V = ProtocolVersion
	}
	data, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// NewScanner returns a bufio.Scanner configured for NDJSON envelopes with the
// frame-size cap applied.
func NewScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), MaxFrameBytes)
	return sc
}

// Decode parses one NDJSON line into an envelope.
func Decode(line []byte) (Envelope, error) {
	if len(line) > MaxFrameBytes {
		return Envelope{}, ErrFrameTooLarge
	}
	var e Envelope
	if err := json.Unmarshal(line, &e); err != nil {
		return Envelope{}, err
	}
	return e, nil
}
