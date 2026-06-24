package wire

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
	BotID       string `json:"botId,omitempty"`
	SessionKey  string `json:"sessionKey"`
	ChannelType int    `json:"channelType,omitempty"`
	Text        string `json:"text"`
	FromUID     string `json:"fromUid,omitempty"`
	FromName    string `json:"fromName,omitempty"`
	Ts          int64  `json:"ts"`
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
	// Cron is true for user rows that originated from the scheduler (cron
	// task fired). The renderer uses it to badge the bubble — preserved
	// through persistence so a chat-window reload doesn't lose the marker
	// on prior fires.
	Cron bool `json:"cron,omitempty"`
	// FromName is the IM platform's display name of the human author of a
	// user-role row, persisted at append time. Carried so a chat-window
	// reload of a multi-author group session can still attribute bubbles
	// to the right speaker (without it, every history bubble would fall
	// back to "You" and the conversation would read like a monologue).
	// Empty for assistant rows and for legacy rows persisted before this
	// field was added.
	FromName string `json:"fromName,omitempty"`
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
	// ChannelName is the IM platform's display name for THIS session's channel:
	// the DM peer's name for a DM, the thread's name for a thread, the group's
	// name for a bare group. The two halves of a thread session ship separately
	// (ParentChannelName carries the parent group) so each GUI surface can
	// compose its own label — sidebar shows the short ChannelName; the chat
	// header reads "<ParentChannelName> > <ChannelName>" for breadcrumb.
	// Empty when the name isn't yet cached; the GUI falls back to the
	// prettified key.
	ChannelName string `json:"channelName,omitempty"`
	// ParentChannelName is the parent group's name for a thread session,
	// empty otherwise. Used by the chat header to render the breadcrumb
	// "<group> > <thread>"; the sidebar ignores it.
	ParentChannelName string `json:"parentChannelName,omitempty"`
}

// SessionUpsertedBody is broadcast as the "session.upserted" event whenever a
// session row's projectable state changes — a new session is persisted, an
// existing one's preview / updatedAt advances after a turn, or its
// channelName resolves for the first time. The GUI upserts the Session row
// into its sidebar without having to re-issue sessions.list. This is the
// push counterpart to the sessions.list pull: list bootstraps, upserted
// keeps the GUI in sync.
type SessionUpsertedBody struct {
	BotID   string         `json:"botId,omitempty"`
	Session SessionSummary `json:"session"`
}
