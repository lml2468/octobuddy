// Package control implements the XClaw control bus: the NDJSON-over-Unix-socket
// protocol (see proto/README.md) between the Go daemon (xclawd) and client
// shells (the desktop app, a web console, a CLI).
//
// The wire contract — envelope, codec, and typed bodies — lives in the
// dependency-free leaf package control/wire so any client can depend on it
// without pulling in agent/gateway internals. This file re-exports that
// vocabulary under the historical `control.*` names and adds the server-side
// pieces (Server, CommandHandler, EventSink) that depend on agent.
package control

import "github.com/lml2468/xclaw/core/control/wire"

// Protocol constants (re-exported from wire).
const (
	ProtocolVersion = wire.ProtocolVersion
	MaxFrameBytes   = wire.MaxFrameBytes
	CmdAuth         = wire.CmdAuth
)

// Kind and its values (re-exported from wire).
type Kind = wire.Kind

const (
	KindCommand  = wire.KindCommand
	KindResponse = wire.KindResponse
	KindEvent    = wire.KindEvent
)

// Envelope and codec (re-exported from wire).
type Envelope = wire.Envelope

// ErrFrameTooLarge is returned when a line exceeds MaxFrameBytes.
var ErrFrameTooLarge = wire.ErrFrameTooLarge

// Encode marshals an envelope to a single NDJSON line (with trailing newline).
func Encode(e Envelope) ([]byte, error) { return wire.Encode(e) }

// NewScanner returns a bufio.Scanner configured for NDJSON envelopes.
var NewScanner = wire.NewScanner

// Decode parses one NDJSON line into an envelope.
func Decode(line []byte) (Envelope, error) { return wire.Decode(line) }

// Typed bodies (re-exported from wire).
type (
	AuthBody             = wire.AuthBody
	SessionSendBody      = wire.SessionSendBody
	SessionHistoryBody   = wire.SessionHistoryBody
	SessionsListBody     = wire.SessionsListBody
	SessionSummary       = wire.SessionSummary
	UsageStatsBody       = wire.UsageStatsBody
	UsageStats           = wire.UsageStats
	SecretInjectBody     = wire.SecretInjectBody
	CronCreateBody       = wire.CronCreateBody
	CronListBody         = wire.CronListBody
	CronDeleteBody       = wire.CronDeleteBody
	CronTaskInfo         = wire.CronTaskInfo
	OKBody               = wire.OKBody
	HealthBody           = wire.HealthBody
	BotInfo              = wire.BotInfo
	SessionTextBody      = wire.SessionTextBody
	SessionToolBody      = wire.SessionToolBody
	SessionUsageBody     = wire.SessionUsageBody
	SessionReplyBody       = wire.SessionReplyBody
	SessionUserMessageBody = wire.SessionUserMessageBody
	SessionActivityBody    = wire.SessionActivityBody
	ErrorBody            = wire.ErrorBody
	HistoryMessage       = wire.HistoryMessage
	HistoryResponse      = wire.HistoryResponse
	SessionsListResponse = wire.SessionsListResponse
)
