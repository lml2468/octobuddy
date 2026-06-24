// Package control implements the OctoBuddy control bus: the NDJSON-over-Unix-socket
// protocol (see proto/README.md) between the Go daemon (octobuddy-daemon) and client
// shells (the desktop app, a web console, a CLI).
//
// The wire contract — envelope, codec, and typed bodies — lives in the
// dependency-free leaf package control/wire so any client can depend on it
// without pulling in agent/gateway internals. This file re-exports that
// vocabulary under the historical `control.*` names and adds the server-side
// pieces (Server, CommandHandler, EventSink) that depend on agent.
package control

import "github.com/lml2468/octobuddy/core/control/wire"

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
	AuthBody               = wire.AuthBody
	SessionSendBody        = wire.SessionSendBody
	SessionAttachment      = wire.SessionAttachment
	SessionHistoryBody     = wire.SessionHistoryBody
	SessionsListBody       = wire.SessionsListBody
	SessionSummary         = wire.SessionSummary
	UsageStatsBody         = wire.UsageStatsBody
	UsageStats             = wire.UsageStats
	SecretInjectBody       = wire.SecretInjectBody
	CronCreateBody         = wire.CronCreateBody
	CronListBody           = wire.CronListBody
	CronDeleteBody         = wire.CronDeleteBody
	CronUpdateBody         = wire.CronUpdateBody
	CronTaskInfo           = wire.CronTaskInfo
	CronListResponse       = wire.CronListResponse
	OKBody                 = wire.OKBody
	HealthBody             = wire.HealthBody
	BotInfo                = wire.BotInfo
	SessionTextBody        = wire.SessionTextBody
	SessionToolBody        = wire.SessionToolBody
	SessionUsageBody       = wire.SessionUsageBody
	SessionReplyBody       = wire.SessionReplyBody
	SessionUserMessageBody = wire.SessionUserMessageBody
	SessionUpsertedBody    = wire.SessionUpsertedBody
	SessionActivityBody    = wire.SessionActivityBody
	ErrorBody              = wire.ErrorBody
	HistoryMessage         = wire.HistoryMessage
	HistoryResponse        = wire.HistoryResponse
	SessionsListResponse   = wire.SessionsListResponse
	MCPCheckBody           = wire.MCPCheckBody
	MCPServerHealth        = wire.MCPServerHealth
	MCPCheckResponse       = wire.MCPCheckResponse
)
