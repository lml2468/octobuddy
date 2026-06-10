import Foundation

/// Supervises the `xclawd` Go core subprocess: launches it with a control
/// socket, watches for exit, and restarts with exponential backoff. The Swift
/// analogue of Open Island's process supervision — here we own the lifecycle of
/// the daemon the GUI drives.
///
/// Lifecycle is reported via the `onState` callback so an AppModel can reflect
/// it. The supervisor does NOT own the control connection; it only guarantees a
/// running daemon at `socketPath`.
public final class CoreSupervisor: @unchecked Sendable {
    public enum State: Equatable, Sendable {
        case stopped
        case starting
        case running(pid: Int32)
        case restarting(afterError: String)
    }

    public struct Config: Sendable {
        /// Absolute path to the `xclawd` binary.
        public var binaryPath: String
        /// Unix socket the daemon will listen on (passed as -control).
        public var socketPath: String
        /// SQLite path (-db). Single-bot mode only; defaults to a temp file.
        public var dbPath: String
        /// Agent driver (-driver). Single-bot mode only.
        public var driver: String
        /// When non-empty, run in multi-bot config mode: `-config <configPath>
        /// -control <socketPath>`. Empty path with configMode=true uses the
        /// daemon's default ~/.xclaw/config.json. Takes precedence over the
        /// single-bot flags above.
        public var configMode: Bool
        public var configPath: String
        /// Extra args appended verbatim.
        public var extraArgs: [String]

        public init(binaryPath: String, socketPath: String, dbPath: String = "",
                    driver: String = "claude", configMode: Bool = false,
                    configPath: String = "", extraArgs: [String] = []) {
            self.binaryPath = binaryPath
            self.socketPath = socketPath
            self.dbPath = dbPath
            self.driver = driver
            self.configMode = configMode
            self.configPath = configPath
            self.extraArgs = extraArgs
        }
    }

    private let config: Config
    private let onState: @Sendable (State) -> Void
    private let queue = DispatchQueue(label: "app.xclaw.supervisor")

    private var process: Process?
    private var stopped = false
    private var backoff: TimeInterval = 1.0
    private static let maxBackoff: TimeInterval = 30.0

    public init(config: Config, onState: @escaping @Sendable (State) -> Void = { _ in }) {
        self.config = config
        self.onState = onState
    }

    /// Starts the daemon and keeps it alive until `stop()`.
    public func start() {
        queue.async { [weak self] in self?.launchLocked() }
    }

    /// Stops the daemon and disables restarts.
    public func stop() {
        queue.async { [weak self] in
            guard let self else { return }
            self.stopped = true
            self.process?.terminationHandler = nil
            self.process?.terminate()
            self.process = nil
            self.onState(.stopped)
        }
    }

    /// Whether the supervised binary exists and is executable.
    public var binaryAvailable: Bool {
        FileManager.default.isExecutableFile(atPath: config.binaryPath)
    }

    private func launchLocked() {
        if stopped { return }
        guard binaryAvailable else {
            // Treat a missing binary as a retryable error so a later install/
            // build is picked up.
            scheduleRestart(error: "xclawd not found at \(config.binaryPath)")
            return
        }

        onState(.starting)

        let p = Process()
        p.executableURL = URL(fileURLWithPath: config.binaryPath)
        var args: [String]
        if config.configMode {
            // Multi-bot: -config [path] -control <sock>. An empty configPath
            // lets the daemon use its default ~/.xclaw/config.json.
            args = ["-config", config.configPath, "-control", config.socketPath]
        } else {
            args = ["-control", config.socketPath, "-no-repl", "-driver", config.driver]
            if !config.dbPath.isEmpty {
                args += ["-db", config.dbPath]
            }
        }
        args += config.extraArgs
        p.arguments = args
        // Inherit stdio so daemon logs surface in the app's console during dev.
        p.standardOutput = FileHandle.standardOutput
        p.standardError = FileHandle.standardError

        p.terminationHandler = { [weak self] proc in
            guard let self else { return }
            self.queue.async {
                if self.stopped { return }
                let reason = "xclawd exited (status \(proc.terminationStatus))"
                self.scheduleRestart(error: reason)
            }
        }

        do {
            try p.run()
        } catch {
            scheduleRestart(error: "launch failed: \(error.localizedDescription)")
            return
        }
        process = p
        backoff = 1.0 // reset backoff on a successful launch
        onState(.running(pid: p.processIdentifier))
    }

    private func scheduleRestart(error: String) {
        if stopped { return }
        process = nil
        onState(.restarting(afterError: error))
        let delay = backoff
        backoff = min(backoff * 2, Self.maxBackoff)
        queue.asyncAfter(deadline: .now() + delay) { [weak self] in
            self?.launchLocked()
        }
    }
}
