import Foundation
import Observation
import XClawCore

/// Central app state: owns the CoreSupervisor (xclawd lifecycle) and the
/// ControlClient (the bus), folds the inbound event stream into an AppState on
/// the main actor, and exposes everything the SwiftUI views render. The XClaw
/// analogue of Open Island's AppModel.
@MainActor
@Observable
final class AppModel {
    // Surfaced to the UI.
    var coreState: String = "stopped"
    var connected: Bool = false
    var bots: [AppState.BotView] = []
    var selectedBotID: String?
    var lastError: String?

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

    /// Boots the core and connects the control bus. Defaults to multi-bot config
    /// mode when ~/.xclaw/config.json exists; otherwise surfaces a needs-config
    /// state (the app shouldn't silently run an empty single-bot daemon).
    func start() {
        CorePaths.ensureSupportDir()

#if DEBUG
        // UI preview/screenshot mode: seed mock data, skip the daemon/bus.
        if ProcessInfo.processInfo.environment["XCLAW_UI_PREVIEW"] != nil {
            state = .preview()
            connected = true
            coreState = "running (preview)"
            publishBots()
            selectedBotID = "main"
            return
        }
#endif

        guard let bin = CorePaths.resolveBinary() else {
            coreState = "error"
            lastError = "xclawd binary not found (set XCLAWD_BIN or build core)"
            return
        }

        let useConfig = CorePaths.configExists
        if !useConfig {
            coreState = "needs-config"
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

    /// Stops the bus and the core.
    func stop() {
        consumeTask?.cancel()
        consumeTask = nil
        pollTask?.cancel()
        pollTask = nil
        client?.disconnect()
        client = nil
        connected = false
        if let sup = supervisor {
            Task { await sup.stop() }
        }
        supervisor = nil
        coreState = "stopped"
    }

    /// Sends a user message to the selected bot over the bus.
    func send(_ text: String) {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty, let client else { return }
        do {
            try client.send(type: "session.send",
                            body: SessionSendBody(uid: localUID, text: trimmed, botId: selectedBotID))
            // Echo our own message into the transcript (the bus doesn't send it back).
            state.appendUserMessage(botId: selectedBotID, sessionKey: localUID, text: trimmed)
            publishBots()
        } catch {
            lastError = "send failed: \(error)"
        }
    }

    /// Clears the selected bot's GUI session resume mapping.
    func reset() {
        guard let client else { return }
        do {
            try client.send(type: "session.reset",
                            body: SessionSendBody(uid: localUID, text: "", botId: selectedBotID))
        } catch {
            Log.app.error("reset failed: \(error.localizedDescription, privacy: .public)")
        }
    }

    // MARK: - private

    private func applyCoreState(_ st: CoreSupervisor.State) {
        switch st {
        case .stopped:
            coreState = "stopped"
            connected = false
        case .starting:
            coreState = "starting"
        case .running(let pid):
            coreState = "running (pid \(pid))"
            // The daemon needs a moment to bind the socket; connect with retry.
            connectWithRetry()
        case .restarting(let err):
            coreState = "restarting"
            lastError = err
            connected = false
            // Drop the stale connection; reconnect after the new daemon is up.
            consumeTask?.cancel()
            pollTask?.cancel()
            client?.disconnect()
            client = nil
        case .failed(let reason):
            coreState = "failed"
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
            do {
                let reqID = try c.send(type: "session.history",
                                       body: SessionHistoryBody(sessionKey: localUID, limit: 200, botId: id))
                pendingHistory[reqID] = (botId: id, sessionKey: localUID)
            } catch {
                historyLoaded.remove(id)
                Log.app.error("session.history request failed: \(error.localizedDescription, privacy: .public)")
            }
        }
    }

    /// Folds a session.history response into the transcript for the session it
    /// was requested for (matched via the correlated command id).
    private func applyHistory(_ env: Envelope) {
        guard let id = env.id, let target = pendingHistory.removeValue(forKey: id),
              let history = env.decodeBody([HistoryMessage].self) else { return }
        let msgs: [AppState.ChatMessage] = history.compactMap { h in
            let role: AppState.ChatMessage.Role? =
                h.role == "user" ? .user : (h.role == "assistant" ? .assistant : nil)
            guard let role else { return nil }
            return AppState.ChatMessage(role: role, text: h.content,
                                        timestamp: Date(timeIntervalSince1970: TimeInterval(h.ts)))
        }
        state.loadHistory(botId: target.botId, sessionKey: target.sessionKey, messages: msgs)
        publishBots()
    }

    /// Moves any plaintext tokens left in config.json into the Keychain, then
    /// rewrites the file without them. No-op once the file is clean.
    private func migrateLegacyTokensIfNeeded() {
        guard let bots = try? ConfigStore.load() else { return }
        var migrated = false
        do {
            for b in bots {
                if !b.octoToken.isEmpty {
                    try Keychain.set(account: Keychain.account(bot: b.id, kind: Keychain.kindOcto), value: b.octoToken)
                    migrated = true
                }
                if !b.gatewayToken.isEmpty {
                    try Keychain.set(account: Keychain.account(bot: b.id, kind: Keychain.kindGateway), value: b.gatewayToken)
                    migrated = true
                }
            }
            if migrated {
                try ConfigStore.save(bots) // save() strips tokens from the file
                Log.keychain.notice("migrated plaintext token(s) from config.json into the Keychain")
            }
        } catch {
            // Leave the file as-is if migration fails; tokens still work as a
            // plaintext fallback. Surface why so it's diagnosable.
            Log.keychain.error("token migration failed: \(error.localizedDescription, privacy: .public)")
        }
    }

    private func startBotPolling(on c: ControlClient) {
        pollTask?.cancel()
        pollTask = Task { @MainActor [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(5))
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
                    self.publishBots()
                case .response where env.type == "bots.list":
                    if let infos = env.decodeBody([BotInfo].self) {
                        self.state.setBots(infos)
                        self.publishBots()
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

    private func publishBots() {
        bots = state.sortedBots
        if selectedBotID == nil {
            selectedBotID = bots.first?.id
        }
    }

    // MARK: - Config editing

    /// Restarts the core to pick up a saved config. (Editing itself lives in
    /// `config` — see ConfigEditorModel.)
    func applyConfigAndRestart() {
        config.needsRestart = false
        stop()
        start()
    }
}
