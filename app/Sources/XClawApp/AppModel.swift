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
    var driver: String = "claude"

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
    /// The DM uid used for messages sent from this GUI.
    @ObservationIgnored let localUID = "gui-user"

    /// Boots the core and connects the control bus. Defaults to multi-bot config
    /// mode when ~/.xclaw/config.json exists; otherwise surfaces a needs-config
    /// state (the app shouldn't silently run an empty single-bot daemon).
    func start(driver: String = "claude") {
        self.driver = driver
        CorePaths.ensureSupportDir()

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
        sup.start()
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
        supervisor?.stop()
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
        } catch {
            lastError = "send failed: \(error)"
        }
    }

    /// Clears the selected bot's GUI session resume mapping.
    func reset() {
        guard let client else { return }
        _ = try? client.send(type: "session.reset",
                             body: SessionSendBody(uid: localUID, text: "", botId: selectedBotID))
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
        _ = try? c.send(type: "health", body: [String: String]())
        _ = try? c.send(type: "bots.list", body: [String: String]())
        startBotPolling(on: c)
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
                    }
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
}
