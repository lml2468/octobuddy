import Foundation

/// The client-side projection of control-bus events — the single source of
/// truth the GUI renders, updated only via `apply(_:)`. Multi-bot aware: events
/// are bucketed by botId (empty botId → the "default" bot in single-bot mode).
public struct AppState: Sendable, Equatable {
    /// One rendered message in a session transcript.
    public struct ChatMessage: Sendable, Equatable, Identifiable {
        public enum Role: Sendable, Equatable { case user, assistant, tool }
        public let id: UUID
        public var role: Role
        public var text: String
        public var timestamp: Date
        public init(id: UUID = UUID(), role: Role, text: String, timestamp: Date = Date()) {
            self.id = id; self.role = role; self.text = text; self.timestamp = timestamp
        }
    }

    public struct SessionView: Sendable, Equatable, Identifiable {
        public var sessionKey: String
        public var lastActivity: String = ""
        public var streamingText: String = ""   // accumulates text deltas this turn
        public var lastReply: String = ""        // assembled reply of the last turn
        public var lastTool: String = ""
        public var inputTokens: Int = 0
        public var outputTokens: Int = 0
        /// Ordered conversation transcript (user / assistant / tool bubbles).
        public var messages: [ChatMessage] = []
        /// Id of the assistant bubble currently being streamed (nil between turns).
        public var streamingAssistant: UUID?
        /// Index of that bubble in `messages`, kept in lockstep with
        /// `streamingAssistant` so text deltas append in O(1) (no per-token
        /// `firstIndex` scan — a hot-path cost during long streamed replies).
        public var streamingAssistantIndex: Int?
        /// True between sending a message and the agent's first output (drives a
        /// "typing…" indicator).
        public var awaitingReply: Bool = false
        public var id: String { sessionKey }

        /// Ends the current streaming assistant bubble (resets id + index in
        /// lockstep). Call wherever a turn boundary closes the bubble.
        public mutating func clearStreaming() {
            streamingAssistant = nil
            streamingAssistantIndex = nil
        }
    }

    public struct BotView: Sendable, Equatable, Identifiable {
        public var id: String
        public var connected: Bool = false
        public var lastError: String?
        public var sessions: [String: SessionView] = [:]

        public var sortedSessions: [SessionView] {
            sessions.values.sorted { $0.sessionKey < $1.sessionKey }
        }
    }

    public private(set) var bots: [String: BotView] = [:]
    public private(set) var connected: Bool = false

    public init() {}

    public var sortedBots: [BotView] { bots.values.sorted { $0.id < $1.id } }

#if DEBUG
    /// A populated state for UI previews/screenshots (no daemon/bus needed).
    public static func preview() -> AppState {
        var s = AppState()
        s.connected = true
        s.setBots([
            BotInfo(id: "main", connected: true, lastError: nil),
            BotInfo(id: "research", connected: false, lastError: "awaiting secret"),
        ])
        s.mutateSession("main", "gui-user") { v in
            v.messages = [
                ChatMessage(role: .user, text: "List the files in the project root and summarize what this repo does."),
                ChatMessage(role: .assistant, text: "I'll check the directory layout first."),
                ChatMessage(role: .tool, text: "Bash · ls -la"),
                ChatMessage(role: .assistant, text: "It's a Go + Swift monorepo: core/ is the xclawd gateway daemon, app/ is this macOS client, and proto/ holds the control-bus contract. Want me to open the README?"),
                ChatMessage(role: .user, text: "yes, and the proto contract too"),
            ]
            v.inputTokens = 1450
            v.outputTokens = 92
            v.lastActivity = "sent"
            v.awaitingReply = true
        }
        // A second session demonstrates the session picker.
        s.mutateSession("main", "alice@acme") { v in
            v.messages = [
                ChatMessage(role: .user, text: "explain the proto control-bus contract"),
                ChatMessage(role: .assistant, text: "Sure — the proto contract is an NDJSON envelope over a Unix domain socket: events out, commands in."),
            ]
        }
        return s
    }
#endif

    public mutating func setConnected(_ v: Bool) { connected = v }

    /// Replaces the bot roster from a bots.list response, preserving any
    /// already-accumulated session views.
    /// Replaces the roster from an authoritative bots.list response: updates
    /// surviving bots in place (preserving their sessions/transcript) and DROPS
    /// any bot no longer present, so a bot removed via Save & Restart doesn't
    /// linger as a ghost. (bot.status updates a single bot in place; only the
    /// full bots.list reconciles membership.)
    public mutating func setBots(_ infos: [BotInfo]) {
        var next: [String: BotView] = [:]
        for info in infos {
            var b = bots[info.id] ?? BotView(id: info.id)
            b.connected = info.connected
            b.lastError = info.lastError
            next[info.id] = b
        }
        bots = next
    }

    /// Applies one decoded control-bus event envelope.
    public mutating func apply(_ env: Envelope) {
        guard env.kind == .event else { return }
        switch env.type {
        case "bot.status":
            if let info = env.decodeBody(BotInfo.self) {
                var b = bots[info.id] ?? BotView(id: info.id)
                b.connected = info.connected
                b.lastError = info.lastError
                bots[info.id] = b
            }
        case "session.text":
            if let x = env.decodeBody(SessionTextBody.self) {
                mutateSession(x.botId, x.sessionKey) { s in
                    s.streamingText += x.delta
                    s.lastActivity = "text"
                    s.awaitingReply = false
                    // Extend the current streaming assistant bubble via its tracked
                    // index (O(1)); fall back to a scan only if the index drifted
                    // (defensive — effectively never hit). Otherwise start a bubble.
                    if let sid = s.streamingAssistant {
                        if let i = s.streamingAssistantIndex,
                           i < s.messages.count, s.messages[i].id == sid {
                            s.messages[i].text += x.delta
                        } else if let i = s.messages.firstIndex(where: { $0.id == sid }) {
                            s.messages[i].text += x.delta
                            s.streamingAssistantIndex = i
                        }
                    } else {
                        let m = ChatMessage(role: .assistant, text: x.delta)
                        s.messages.append(m)
                        s.streamingAssistant = m.id
                        s.streamingAssistantIndex = s.messages.count - 1
                    }
                }
            }
        case "session.tool":
            if let x = env.decodeBody(SessionToolBody.self) {
                mutateSession(x.botId, x.sessionKey) { s in
                    s.lastTool = x.name
                    s.lastActivity = "tool:\(x.name)"
                    s.awaitingReply = false
                    let label = x.params.isEmpty ? x.name : "\(x.name) \(x.params)"
                    s.messages.append(ChatMessage(role: .tool, text: label))
                    // A tool call ends the current text bubble; later text starts a new one.
                    s.clearStreaming()
                }
            }
        case "session.usage":
            if let x = env.decodeBody(SessionUsageBody.self) {
                mutateSession(x.botId, x.sessionKey) { s in
                    s.inputTokens = x.inputTokens
                    s.outputTokens = x.outputTokens
                }
            }
        case "session.activity":
            if let x = env.decodeBody(SessionActivityBody.self) {
                mutateSession(x.botId, x.sessionKey) { s in
                    s.lastActivity = x.kind
                    if x.kind == "turnStart" {
                        s.streamingText = ""
                        s.clearStreaming()
                        s.awaitingReply = true
                    } else if x.kind == "turnDone" {
                        s.awaitingReply = false
                    }
                }
            }
        case "session.reply":
            if let x = env.decodeBody(SessionReplyBody.self) {
                mutateSession(x.botId, x.sessionKey) { s in
                    s.lastReply = x.text
                    s.lastActivity = "reply"
                    s.awaitingReply = false
                    // Fallback for drivers that only emit a final reply (no text
                    // deltas): if no assistant output landed this turn, add it.
                    if s.messages.last?.role == .user, !x.text.isEmpty {
                        s.messages.append(ChatMessage(role: .assistant, text: x.text))
                    }
                    s.clearStreaming()
                }
            }
        case "error":
            if let x = env.decodeBody(ErrorBody.self) {
                let id = botKey(x.botId)
                var b = bots[id] ?? BotView(id: id)
                b.lastError = x.message
                bots[id] = b
            }
        default:
            break
        }
    }

    // botKey maps an empty botId (single-bot mode) to a stable "default" bucket.
    private func botKey(_ botId: String?) -> String {
        let id = botId ?? ""
        return id.isEmpty ? "default" : id
    }

    /// Records a locally-sent user message as a bubble in the transcript (the
    /// control bus doesn't echo our own outgoing message back as an event).
    public mutating func appendUserMessage(botId: String?, sessionKey: String, text: String) {
        mutateSession(botId, sessionKey) { s in
            s.messages.append(ChatMessage(role: .user, text: text))
            s.lastActivity = "sent"
            s.clearStreaming()
            s.awaitingReply = true
        }
    }

    /// Hydrates a session's transcript from persisted history (session.history).
    /// No-op if the session already has live messages, so it never clobbers an
    /// in-progress conversation.
    public mutating func loadHistory(botId: String?, sessionKey: String, messages: [ChatMessage]) {
        guard !messages.isEmpty else { return }
        let id = botKey(botId)
        var b = bots[id] ?? BotView(id: id)
        var s = b.sessions[sessionKey] ?? SessionView(sessionKey: sessionKey)
        guard s.messages.isEmpty else { return }
        s.messages = messages
        b.sessions[sessionKey] = s
        bots[id] = b
    }

    private mutating func mutateSession(_ botId: String?, _ sessionKey: String, _ f: (inout SessionView) -> Void) {
        let id = botKey(botId)
        var b = bots[id] ?? BotView(id: id)
        var s = b.sessions[sessionKey] ?? SessionView(sessionKey: sessionKey)
        f(&s)
        b.sessions[sessionKey] = s
        bots[id] = b
    }
}
