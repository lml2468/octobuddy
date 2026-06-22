// Package wire defines the XClaw control-bus contract: the NDJSON envelope, its
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

// --- typed bodies (proto/README.md) ---

// Commands (client → server)

// AuthBody presents the GUI capability token (proto: auth). The server compares
// it in constant time against the token it was minted with at spawn; a match
// marks the connection authorized for the privileged command set. The token is
// delivered to the GUI out-of-band (a private fd the spawned agent never sees),
// held in daemon memory only, and is NEVER logged or persisted.
type AuthBody struct {
	Token string `json:"token"`
}

type SessionSendBody struct {
	// BotID selects which bot to route to (multi-bot config mode). Empty = the
	// single/default bot.
	BotID string `json:"botId,omitempty"`
	// UID is the DM uid; the server routes it as a DM inbound for the MVP.
	UID  string `json:"uid"`
	Text string `json:"text"`
}

type SessionHistoryBody struct {
	BotID      string `json:"botId,omitempty"`
	SessionKey string `json:"sessionKey"`
	Limit      int    `json:"limit"`
}

// SessionsListBody requests every persisted session for a bot (proto:
// sessions.list), newest updated first, for the desktop conversation list.
type SessionsListBody struct {
	BotID string `json:"botId,omitempty"`
}

// UsageStatsBody requests a bot's token usage (proto: usage.stats) for the
// desktop Token Usage window. Since (Unix seconds) bounds the range: 0 = all
// time; otherwise only day buckets at or after Since are summed. The client
// computes Since from its own local calendar (today / last 7d / …) so the range
// matches the user's timezone, not the daemon's.
type UsageStatsBody struct {
	BotID string `json:"botId,omitempty"`
	Since int64  `json:"since,omitempty"`
}

// UsageStats is the usage.stats response: a bot's cumulative token totals across
// every completed turn (persisted, so it survives restarts).
type UsageStats struct {
	BotID            string  `json:"botId,omitempty"`
	Since            int64   `json:"since"` // echoes the request range bound (0 = all time)
	InputTokens      int64   `json:"inputTokens"`
	OutputTokens     int64   `json:"outputTokens"`
	CachedTokens     int64   `json:"cachedTokens"`
	CacheWriteTokens int64   `json:"cacheWriteTokens"`
	CostUSD          float64 `json:"costUsd"`
	Turns            int64   `json:"turns"`
}

// SecretKind enumerates the categories of secret carried over secret.inject.
// Owned by wire so both ends of the bus (and any future tool) refer to one
// canonical contract instead of duplicating string literals at the call site.
// String-typed (rather than an int) so the JSON shape stays human-readable
// and the wire surface doesn't have to grow an enum codec.
type SecretKind string

const (
	SecretKindOcto    SecretKind = "octoToken"
	SecretKindGateway SecretKind = "gatewayToken"
)

// SecretInjectBody carries a single secret into the core (proto: secret.inject).
// The value is held in memory only — never persisted, never logged.
type SecretInjectBody struct {
	BotID string     `json:"botId,omitempty"`
	Kind  SecretKind `json:"kind"`
	Value string     `json:"value"`
	// Clear, when true, explicitly removes the stored token for Kind (the GUI's
	// "log out / clear credentials" action). Without it an empty Value is ignored,
	// so seeding from an absent config field never clobbers an injected token.
	Clear bool `json:"clear,omitempty"`
}

// CronCreateBody registers a scheduled task (proto: cron.create). Owner-gated on
// the SERVER-resolved owner uid, not on any field here — the body uid is not an
// authorization claim (it is forgeable; the agent reaches cron over an
// agent-controlled CLI). The created task BINDS to the channel coords given
// here: a channelId (group) targets that channel; omitting it targets the
// owner's DM. The fired prompt always runs as the owner.
type CronCreateBody struct {
	BotID string `json:"botId,omitempty"`
	// UID is accepted for proto compatibility but IGNORED for authorization and
	// for DM binding (the resolved owner is used for both). Deprecated.
	UID string `json:"uid,omitempty"`
	// Schedule is a 5-field cron expr ("0 9 * * 1-5") or one-shot ISO datetime.
	Schedule string `json:"schedule"`
	// Prompt is the instruction injected when the task fires (≤ 2048 bytes).
	Prompt string `json:"prompt"`
	// Recurring, when set, overrides the default (cron→true, one-shot→false).
	Recurring *bool `json:"recurring,omitempty"`
	// ChannelID + ChannelType bind a GROUP task. Omit (or type 1) for a DM task,
	// which binds to the resolved owner. ChannelType: 1 = DM, 2 = Group, 3 = Console.
	ChannelID   string `json:"channelId,omitempty"`
	ChannelType int    `json:"channelType,omitempty"`
	// FromUID identifies WHO the task fires AS — distinct from the auth uid
	// (which is server-resolved + not from this body). For DM targets this is
	// the peer's uid (the task fires as a DM from the bot to that peer). For
	// Console targets the handler stamps cron.ConsoleUID regardless of the body.
	// For Group targets the handler stamps the owner (the bot identifies as
	// itself in the group). Empty for DM is a validation error at create time;
	// empty for DM on update preserves the existing FromUID (the "blank =
	// preserve" GUI contract for the edit modal).
	FromUID  string `json:"fromUid,omitempty"`
	FromName string `json:"fromName,omitempty"`
}

// CronListBody lists a bot's scheduled tasks (proto: cron.list).
type CronListBody struct {
	BotID string `json:"botId,omitempty"`
}

// CronDeleteBody removes a task by id (proto: cron.delete). Owner-gated on the
// server-resolved owner uid; the body carries no authorization claim.
type CronDeleteBody struct {
	BotID string `json:"botId,omitempty"`
	// UID is accepted for proto compatibility but IGNORED for authorization.
	UID string `json:"uid,omitempty"`
	ID  string `json:"id"`
}

// CronUpdateBody mutates an existing task by id (proto: cron.update). Same
// fields as CronCreateBody plus ID, with an optional Enabled toggle. Editing
// is a full replacement of mutable fields — partial PATCH would multiply the
// schema-mismatch surface and confuse "did the schedule change or not"
// audits. Enabled is a pointer so the GUI's toggle UX can send
// enabled-only updates without echoing schedule/prompt/channel back.
//
// Owner-gated on the server-resolved owner uid (same model as create + delete):
// the task is only updatable by the bot's current owner, AND only if the task's
// CreatedBy matches that owner — a task created under a previous owner uid
// (pre-token-rotation) is invisible / immutable to the new owner.
type CronUpdateBody struct {
	BotID       string `json:"botId,omitempty"`
	ID          string `json:"id"`
	Schedule    string `json:"schedule,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	Recurring   *bool  `json:"recurring,omitempty"`
	ChannelID   string `json:"channelId,omitempty"`
	ChannelType int    `json:"channelType,omitempty"`
	// FromUID — see CronCreateBody.FromUID. On update an empty value PRESERVES
	// the existing stored FromUID (so the GUI's "blank = preserve" edit-modal
	// contract is honored at the wire layer, not silently rebound by the
	// handler stamping owner over the peer uid).
	FromUID  string `json:"fromUid,omitempty"`
	FromName string `json:"fromName,omitempty"`
	// Enabled, when non-nil, sets the task's Enabled flag. nil leaves it.
	// Sent alone (no other field) by the GUI's per-row enable/disable toggle
	// so the round-trip is minimal.
	Enabled *bool `json:"enabled,omitempty"`
}

// CronTaskInfo is a task rendered for clients (nextRun as ISO; no internal churn).
// CreatedBy / FromUID are deliberately omitted — operator-internal auth state,
// not for the renderer to display or echo back.
type CronTaskInfo struct {
	ID          string `json:"id"`
	Schedule    string `json:"schedule"`
	Recurring   bool   `json:"recurring"`
	Prompt      string `json:"prompt"`
	NextRun     string `json:"nextRun,omitempty"`     // RFC3339, empty when none
	LastRun     string `json:"lastRun,omitempty"`     // RFC3339, empty when never fired
	ChannelID   string `json:"channelId,omitempty"`   // empty for DM/Console targets
	ChannelType int    `json:"channelType,omitempty"` // 1=DM, 2=Group, 3=Console
	FromName    string `json:"fromName,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// CronListResponse is the cron.list response, tagged with the botId the
// request was about. Mirrors SessionsListResponse — the wrapper carries
// botId so the renderer can route the response to the right bot's local
// schedules map even if the user has switched bots mid-fetch (the
// envelope event handler has no other channel for that correlation).
type CronListResponse struct {
	BotID string         `json:"botId"`
	Tasks []CronTaskInfo `json:"tasks"`
}

// Responses / event bodies (server → client)

type OKBody struct {
	OK bool `json:"ok"`
}

type HealthBody struct {
	Uptime      int64  `json:"uptime"`
	Connections int    `json:"connections"`
	Driver      string `json:"driver"`
	Bots        int    `json:"bots"`
}

// BotInfo describes one bot for the bots.list response and bot.status events.
type BotInfo struct {
	ID        string `json:"id"`
	Connected bool   `json:"connected"`
	LastError string `json:"lastError,omitempty"`
}

type SessionTextBody struct {
	BotID      string `json:"botId,omitempty"`
	SessionKey string `json:"sessionKey"`
	Delta      string `json:"delta"`
}

type SessionToolBody struct {
	BotID      string `json:"botId,omitempty"`
	SessionKey string `json:"sessionKey"`
	Name       string `json:"name"`
	Params     string `json:"params"`
}

type SessionUsageBody struct {
	BotID             string  `json:"botId,omitempty"`
	SessionKey        string  `json:"sessionKey"`
	InputTokens       int     `json:"inputTokens"`
	OutputTokens      int     `json:"outputTokens"`
	CachedInputTokens int     `json:"cachedInputTokens,omitempty"`
	CostUSD           float64 `json:"costUsd,omitempty"`
}

type SessionReplyBody struct {
	BotID      string `json:"botId,omitempty"`
	SessionKey string `json:"sessionKey"`
	Text       string `json:"text"`
}

// SessionUserMessageBody is broadcast at the start of each accepted turn so
// observer clients (the desktop GUI) can render the inbound user message in
// the chat transcript. Without this, an IM-originated session in the GUI only
// showed the bot's reply and read like a monologue. FromUID + FromName let
// the GUI pick the right avatar / name for group chats where multiple humans
// share one session. Ts is the server's accept time (seconds since epoch);
// the GUI uses it for the "X minutes ago" label and ordering. Console-
// originated turns also emit this — the GUI dedupes against the message it
// optimistically pushed when the Composer typed it.
type SessionUserMessageBody struct {
	BotID      string `json:"botId,omitempty"`
	SessionKey string `json:"sessionKey"`
	Text       string `json:"text"`
	FromUID    string `json:"fromUid,omitempty"`
	FromName   string `json:"fromName,omitempty"`
	Ts         int64  `json:"ts"`
	// CronFire is true when this user_message represents a scheduled-task
	// trigger rather than a real human inbound. The renderer uses it to (a)
	// override the Composer-typed dedupe — a cron Console fire shares the
	// CONSOLE_UID sessionKey but has NO optimistic local push to dedupe
	// against, so the existing "skip CONSOLE_UID" path would otherwise hide
	// the prompt — and (b) badge the bubble with a "[定时任务]" prefix so the
	// operator can tell at a glance that a message came from the scheduler.
	CronFire bool `json:"cronFire,omitempty"`
}

type SessionActivityBody struct {
	BotID      string `json:"botId,omitempty"`
	SessionKey string `json:"sessionKey"`
	Kind       string `json:"kind"`
}

type ErrorBody struct {
	BotID       string `json:"botId,omitempty"`
	Scope       string `json:"scope"`
	Message     string `json:"message"`
	Recoverable bool   `json:"recoverable"`
}

type HistoryMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	TS      int64  `json:"ts"`
}

// HistoryResponse is the session.history response. It echoes the requested botId
// and session key so the client can route the rows to the right session even if
// the user switched sessions while the fetch was in flight (avoids the
// land-on-wrong-session race).
type HistoryResponse struct {
	BotID    string           `json:"botId"`
	Key      string           `json:"key"`
	Messages []HistoryMessage `json:"messages"`
}

// SessionsListResponse is the sessions.list response, tagged with the botId the
// rows belong to so the client never folds them into the wrong bot if the user
// switched bots while the fetch was in flight.
type SessionsListResponse struct {
	BotID    string           `json:"botId"`
	Sessions []SessionSummary `json:"sessions"`
}

// SessionSummary is one row of the sessions.list response: a persisted session
// plus a preview from its latest message (empty when it has none).
type SessionSummary struct {
	Key         string `json:"key"`
	ChannelType int    `json:"channelType"`
	UpdatedAt   int64  `json:"updatedAt"` // Unix seconds
	Preview     string `json:"preview"`
	LastRole    string `json:"lastRole"`
}
