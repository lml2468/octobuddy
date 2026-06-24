package wire

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
	// Attachments are Composer-side files (image / file) attached to this send.
	// Bytes arrive base64-encoded over the control bus; the daemon writes them
	// to the session sandbox via gateway/media's writers and appends the
	// matching prompt fragment (same shape as IM-inbound media) to Text before
	// driver.Query — so the agent sees identical structure regardless of
	// whether a file came from an IM peer or the operator's desktop.
	Attachments []SessionAttachment `json:"attachments,omitempty"`
}

// SessionAttachment is one Composer-side attachment on a session.send.
// Daemon-side caps mirror gateway/media constants (MaxImageBytes /
// MaxFileBytes / MaxImagesPerSend) — over-cap entries are rejected at
// write time, not silently truncated.
type SessionAttachment struct {
	// Name is the operator-visible filename (sanitized client-side for
	// display; the daemon re-sanitizes for the on-disk leaf).
	Name string `json:"name"`
	// Kind picks the prompt-fragment template: "image" routes through the
	// image writer + image-Read hint, "file" through the file writer +
	// inline-or-download branch.
	Kind string `json:"kind"`
	// Mime is the client-detected content type, advisory. For images the
	// daemon uses it to pick the extension (PNG/JPEG/GIF/WebP); for files
	// it's ignored.
	Mime string `json:"mime,omitempty"`
	// Data is the base64-encoded file bytes.
	Data string `json:"data"`
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
	// SecretKindEnvPrefix prefixes per-bot agent env secrets. Example:
	// "env/GH_TOKEN" is injected into the env declaration whose secretRef
	// is "env/GH_TOKEN".
	SecretKindEnvPrefix = "env/"
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

// MCPCheckBody is the mcp.check request: probe the addressed bot's currently
// saved .mcp.json (under its CLAUDE_CONFIG_DIR) and report each server's
// health. Backs the desktop's MCP "test connection" button + post-save check.
type MCPCheckBody struct {
	BotID string `json:"botId"`
}

// MCPServerHealth is one MCP server's probed state: Status is "connected" /
// "failed" (claude's own wording from the system/init line); Tools are the
// mcp__<name>__* tools it contributed (empty when failed).
type MCPServerHealth struct {
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Tools  []string `json:"tools"`
}

// MCPCheckResponse is the mcp.check response, tagged with botId for routing.
// Configured is false when the bot has no .mcp.json at all (so the UI can say
// "no MCP servers configured" rather than "all failed").
type MCPCheckResponse struct {
	BotID      string            `json:"botId"`
	Configured bool              `json:"configured"`
	Servers    []MCPServerHealth `json:"servers"`
}
