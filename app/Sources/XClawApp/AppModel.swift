import Foundation
import Observation
import XClawCore

/// UI-facing core lifecycle state. Mirrors `CoreSupervisor.State` plus the two
/// app-level conditions the supervisor doesn't model (`needsConfig`, `preview`),
/// so views switch on a closed set instead of comparing magic strings.
enum CoreUIState: Equatable {
    case stopped
    case starting
    case restarting
    case needsConfig
    case preview
    case running(pid: Int)
    case failed(String)
    case error(String)

    /// Human-readable status used as the subtitle fallback.
    var display: String {
        switch self {
        case .stopped: return "stopped"
        case .starting: return "starting"
        case .restarting: return "restarting"
        case .needsConfig: return "needs configuration"
        case .preview: return "running (preview)"
        case .running(let pid): return "running (pid \(pid))"
        case .failed: return "failed"
        case .error: return "error"
        }
    }
}

/// A lean, structural summary of one bot for the sidebar. It changes only on
/// membership/connection/error events — never on streamed text — so the sidebar
/// is not invalidated per token (the core of the freeze fix).
struct BotRosterItem: Identifiable, Equatable, Sendable {
    let id: String
    var connected: Bool
    var lastError: String?
    var sessionCount: Int
}

/// Central app state: owns the CoreSupervisor (xclawd lifecycle) and the
/// ControlClient (the bus), folds the inbound event stream into an AppState on
/// the main actor, and exposes everything the SwiftUI views render. The XClaw
/// analogue of Open Island's AppModel.
@MainActor
@Observable
final class AppModel {
    // Surfaced to the UI.
    var coreState: CoreUIState = .stopped
    var connected: Bool = false
    var bots: [AppState.BotView] = []
    var selectedBotID: String?
    var lastError: String?

    /// Lean structural roster for the sidebar (no per-token churn).
    var roster: [BotRosterItem] = []
    /// Selected conversation (session) within the selected bot.
    var selectedSessionKey: String?
    /// Bot ids whose transcript history is currently being fetched (drives the
    /// loading skeleton). Cleared when the history response lands.
    var historyLoadingBots: Set<String> = []

    /// The selected bot's subtree, derived from the published `bots` tree (which
    /// refreshes at ~30fps during streaming). Computed — no per-token storage.
    var currentBot: AppState.BotView? { bots.first { $0.id == selectedBotID } }

    /// The selected conversation, falling back to the bot's first session.
    var currentSession: AppState.SessionView? {
        guard let b = currentBot else { return nil }
        let ss = b.sortedSessions
        return ss.first { $0.sessionKey == selectedSessionKey } ?? ss.first
    }

    /// Bot-configuration editor state (Settings). Separate concern from the
    /// runtime connection/supervision this model owns.
    let config = ConfigEditorModel()

    /// Sessions of the currently-selected bot (convenience for the UI).
    var sessions: [AppState.SessionView] {
        guard let id = selectedBotID, let b = bots.first(where: { $0.id == id }) else {
            return bots.first?.sortedSessions ?? []
        }
        return b.sortedSessions
    }

    @ObservationIgnored private var supervisor: CoreSupervisor?
    @ObservationIgnored private var client: ControlClient?
    @ObservationIgnored private var consumeTask: Task<Void, Never>?
    @ObservationIgnored private var pollTask: Task<Void, Never>?
    @ObservationIgnored private var state = AppState()
    @ObservationIgnored private let socketPath = CorePaths.socketPath
    /// Outstanding session.history requests: command id → (botId, sessionKey).
    @ObservationIgnored private var pendingHistory: [String: (botId: String, sessionKey: String)] = [:]
    /// Bot sessions already hydrated from history, so we request each once.
    @ObservationIgnored private var historyLoaded: Set<String> = []
    /// The DM uid used for messages sent from this GUI.
    @ObservationIgnored let localUID = "gui-user"

    // Coalesced UI publishing: inbound events fold into `state` at full speed,
    // but we re-publish to SwiftUI at most once per `flushInterval` (~30fps),
    // so a streamed reply of hundreds of tokens collapses to ≤1 publish/frame.
    @ObservationIgnored private var rosterDirty = false
    @ObservationIgnored private var transcriptDirty = false
    @ObservationIgnored private var flushScheduled = false
    @ObservationIgnored private let flushInterval: Duration = .milliseconds(33)

    /// Boots the core and connects the control bus. Defaults to multi-bot config
    /// mode when ~/.xclaw/config.json exists; otherwise surfaces a needs-config
    /// state (the app shouldn't silently run an empty single-bot daemon).
    func start() {
        CorePaths.ensureSupportDir()

#if DEBUG
        // UI preview/screenshot mode: seed mock data, skip the daemon/bus.
        if ProcessInfo.processInfo.environment["XCLAW_UI_PREVIEW"] != nil {
            state = .preview()
            config.bots = [
                BotConfig(id: "main", apiURL: "https://octo.acme.example",
                          model: "claude-opus-4-8",
                          gatewayBaseURL: "https://gw.acme.example/v1",
                          env: ["OCTO_BOT_ID": "main-7f3a", "GH_TOKEN": "ghp_…"],
                          soul: "You are Atlas, the team's ops copilot. Calm, terse, precise.",
                          agents: "Confirm before destructive actions. Prefer links over long pastes."),
                BotConfig(id: "research", apiURL: "https://octo.acme.example"),
            ]
            connected = true
            coreState = .preview
            selectedBotID = "main"
            rosterDirty = true; transcriptDirty = true
            flush()
            return
        }
#endif

        guard let bin = CorePaths.resolveBinary() else {
            coreState = .error("xclawd binary not found")
            lastError = "xclawd binary not found (set XCLAWD_BIN or build core)"
            return
        }

        let useConfig = CorePaths.configExists
        if !useConfig {
            coreState = .needsConfig
            lastError = "No config at \(CorePaths.configPath). Create it (see config.example.json) to run bots."
            return
        }

        // Migrate any plaintext tokens still in config.json into the Keychain,
        // then rewrite the file without them (one-time, automatic).
        config.migrateLegacyTokens()

        let cfg = CoreSupervisor.Config(
            binaryPath: bin,
            socketPath: socketPath,
            configMode: true,
            configPath: CorePaths.configPath
        )
        let sup = CoreSupervisor(config: cfg) { [weak self] st in
            // Called off the main actor; hop back to update @Observable state.
            Task { @MainActor [weak self] in self?.applyCoreState(st) }
        }
        supervisor = sup
        Task { await sup.start() }
    }

    /// Stops the bus and the core (fire-and-forget teardown, e.g. on quit).
    func stop() {
        teardownBus()
        if let sup = supervisor {
            Task { await sup.stop() }
        }
        supervisor = nil
        coreState = .stopped
    }

    /// Tears down the bus connection + background tasks (shared by stop/restart).
    private func teardownBus() {
        consumeTask?.cancel()
        consumeTask = nil
        pollTask?.cancel()
        pollTask = nil
        client?.disconnect()
        client = nil
        connected = false
        // Allow history to be re-requested on the next connect (a fresh client
        // restarts its command-id counter, so stale correlations must not carry
        // over).
        historyLoaded.removeAll()
        pendingHistory.removeAll()
        historyLoadingBots.removeAll()
        // Publish any pending coalesced state so the final transcript lands.
        flush()
    }

    /// Restarts the core, fully stopping the current daemon (and waiting for it
    /// to exit) before launching the next one. Sequencing is required: the
    /// daemon removes its control socket on exit, so overlapping a dying daemon
    /// with a fresh one lets the old daemon's cleanup delete the new daemon's
    /// socket — leaving the GUI unable to reconnect.
    func restartCore() {
        teardownBus()
        coreState = .restarting
        let old = supervisor
        supervisor = nil
        Task { @MainActor in
            await old?.stop()
            start()
        }
    }

    /// Sends a user message to the selected bot over the bus.
    func send(_ text: String) {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty, let client else { return }
        // Non-blocking send: the socket write runs on the client's write queue so
        // a full kernel buffer can't beachball the main actor.
        client.sendAsync(type: "session.send",
                         body: SessionSendBody(uid: localUID, text: trimmed, botId: selectedBotID))
        // Echo our own message into the transcript (the bus doesn't send it back).
        state.appendUserMessage(botId: selectedBotID, sessionKey: localUID, text: trimmed)
        markTranscriptDirty()
    }

    /// Clears the selected bot's GUI session resume mapping.
    func reset() {
        guard let client else { return }
        client.sendAsync(type: "session.reset",
                         body: SessionSendBody(uid: localUID, text: "", botId: selectedBotID))
    }

    // MARK: - private

    private func applyCoreState(_ st: CoreSupervisor.State) {
        switch st {
        case .stopped:
            coreState = .stopped
            connected = false
        case .starting:
            coreState = .starting
        case .running(let pid):
            coreState = .running(pid: Int(pid))
            // The daemon needs a moment to bind the socket; connect with retry.
            connectWithRetry()
        case .restarting(let err):
            coreState = .restarting
            lastError = err
            connected = false
            // Drop the stale connection; reconnect after the new daemon is up.
            consumeTask?.cancel()
            pollTask?.cancel()
            client?.disconnect()
            client = nil
        case .failed(let reason):
            coreState = .failed(reason)
            lastError = reason
            connected = false
            consumeTask?.cancel()
            pollTask?.cancel()
            client?.disconnect()
            client = nil
        }
    }

    private func connectWithRetry(attempt: Int = 0) {
        // Already connected.
        if client != nil { return }
        let c = ControlClient(path: socketPath)
        do {
            try c.connect()
        } catch {
            guard attempt < 20 else {
                lastError = "control connect failed: \(error)"
                return
            }
            // Socket may not be bound yet; retry shortly.
            Task { @MainActor [weak self] in
                try? await Task.sleep(for: .milliseconds(150))
                self?.connectWithRetry(attempt: attempt + 1)
            }
            return
        }
        client = c
        connected = true
        consumeEvents(from: c)
        // Probe health and fetch the bot roster immediately, then poll it.
        do {
            try c.send(type: "health", body: [String: String]())
            try c.send(type: "bots.list", body: [String: String]())
        } catch {
            Log.app.error("initial probe failed: \(error.localizedDescription, privacy: .public)")
        }
        injectSecrets(on: c)
        startBotPolling(on: c)
    }

    /// Sends each configured bot's tokens (Keychain, with a config-file fallback
    /// for headless setups) to the core via secret.inject. The core holds them in
    /// memory only; a bot waiting on "awaiting secret" connects once injected.
    private func injectSecrets(on c: ControlClient) {
        let bots = (try? ConfigStore.load()) ?? []
        for b in bots {
            inject(on: c, botID: b.id, kind: Keychain.kindOcto,
                   value: Keychain.get(account: Keychain.account(bot: b.id, kind: Keychain.kindOcto)) ?? b.octoToken)
            inject(on: c, botID: b.id, kind: Keychain.kindGateway,
                   value: Keychain.get(account: Keychain.account(bot: b.id, kind: Keychain.kindGateway)) ?? b.gatewayToken)
        }
    }

    private func inject(on c: ControlClient, botID: String, kind: String, value: String) {
        guard !value.isEmpty else { return }
        do {
            try c.send(type: "secret.inject", body: SecretInjectBody(botId: botID, kind: kind, value: value))
        } catch {
            Log.app.error("secret.inject(\(kind, privacy: .public)) failed: \(error.localizedDescription, privacy: .public)")
        }
    }

    /// Requests each bot's GUI-session transcript from the gateway's store
    /// (session.history) so a restart restores the conversation. Each bot's
    /// session is requested once; the response is matched back by command id.
    private func requestHistories(on c: ControlClient, for botIDs: [String]) {
        for id in botIDs where !historyLoaded.contains(id) {
            historyLoaded.insert(id)
            historyLoadingBots.insert(id)
            do {
                let reqID = try c.send(type: "session.history",
                                       body: SessionHistoryBody(sessionKey: localUID, limit: 200, botId: id))
                pendingHistory[reqID] = (botId: id, sessionKey: localUID)
            } catch {
                historyLoaded.remove(id)
                historyLoadingBots.remove(id)
                Log.app.error("session.history request failed: \(error.localizedDescription, privacy: .public)")
            }
        }
    }

    /// Folds a session.history response into the transcript for the session it
    /// was requested for (matched via the correlated command id).
    private func applyHistory(_ env: Envelope) {
        guard let id = env.id, let target = pendingHistory.removeValue(forKey: id) else { return }
        historyLoadingBots.remove(target.botId)
        markTranscriptDirty()
        guard let history = env.decodeBody([HistoryMessage].self) else { return }
        let msgs: [AppState.ChatMessage] = history.compactMap { h in
            let role: AppState.ChatMessage.Role? =
                h.role == "user" ? .user : (h.role == "assistant" ? .assistant : nil)
            guard let role else { return nil }
            return AppState.ChatMessage(role: role, text: h.content,
                                        timestamp: Date(timeIntervalSince1970: TimeInterval(h.ts)))
        }
        state.loadHistory(botId: target.botId, sessionKey: target.sessionKey, messages: msgs)
        markTranscriptDirty()
    }

    private func startBotPolling(on c: ControlClient) {
        pollTask?.cancel()
        pollTask = Task { @MainActor [weak self] in
            while !Task.isCancelled {
                // The core pushes `bot.status` broadcasts on every change, so this
                // is only a slow reconciliation fallback (missed pushes / clock
                // drift) — not the primary update path.
                try? await Task.sleep(for: .seconds(15))
                guard let self, self.client === c else { return }
                _ = try? c.send(type: "bots.list", body: [String: String]())
            }
        }
    }

    private func consumeEvents(from c: ControlClient) {
        consumeTask?.cancel()
        consumeTask = Task { @MainActor [weak self] in
            for await env in c.events {
                guard let self else { return }
                switch env.kind {
                case .event:
                    self.state.apply(env)
                    // Route to the right (coalesced) publish: status/error touch the
                    // sidebar roster; session.* events only touch the transcript.
                    if env.type == "bot.status" || env.type == "error" {
                        self.markRosterDirty()
                    } else {
                        self.markTranscriptDirty()
                    }
                case .response where env.type == "bots.list":
                    if let infos = env.decodeBody([BotInfo].self) {
                        self.state.setBots(infos)
                        // Drop a selection that points at a bot no longer in the
                        // roster (e.g. removed via Save & Restart).
                        let ids = Set(infos.map(\.id))
                        if let sel = self.selectedBotID, !ids.contains(sel) {
                            self.selectedBotID = infos.first?.id
                        } else if self.selectedBotID == nil {
                            self.selectedBotID = infos.first?.id
                        }
                        self.markRosterDirty()
                        self.requestHistories(on: c, for: infos.map(\.id))
                    }
                case .response where env.type == "session.history":
                    self.applyHistory(env)
                default:
                    break
                }
            }
            // Stream ended (disconnect); reflect it.
            self?.connected = false
        }
    }

    // MARK: - coalesced publishing

    private func markRosterDirty() { rosterDirty = true; scheduleFlush() }
    private func markTranscriptDirty() { transcriptDirty = true; scheduleFlush() }

    /// Arms a single deferred flush (~30fps). Repeated marks within the window
    /// collapse into one publish, so hundreds of streamed tokens don't each
    /// trigger a SwiftUI re-render.
    private func scheduleFlush() {
        guard !flushScheduled else { return }
        flushScheduled = true
        Task { @MainActor [weak self] in
            try? await Task.sleep(for: self?.flushInterval ?? .milliseconds(33))
            self?.flush()
        }
    }

    /// Publishes pending state to SwiftUI: re-publishes the bot tree at most once
    /// per call, and refreshes the lean `roster` only when structure/status
    /// changed. `currentBot` tracks the selection for the (Stage-4) detail view.
    private func flush() {
        flushScheduled = false
        guard rosterDirty || transcriptDirty else { return }
        bots = state.sortedBots
        if rosterDirty {
            roster = bots.map {
                BotRosterItem(id: $0.id, connected: $0.connected,
                              lastError: $0.lastError, sessionCount: $0.sessions.count)
            }
            if selectedBotID == nil { selectedBotID = bots.first?.id }
        }
        rosterDirty = false
        transcriptDirty = false
    }

    // MARK: - Config editing

    /// Restarts the core to pick up a saved config. (Editing itself lives in
    /// `config` — see ConfigEditorModel.)
    func applyConfigAndRestart() {
        config.needsRestart = false
        restartCore()
    }
}
