import Foundation

/// Supervises the `xclawd` Go core subprocess: launches it with a control
/// socket, watches for exit, and restarts with exponential backoff. The Swift
/// analogue of Open Island's process supervision — here we own the lifecycle of
/// the daemon the GUI drives.
///
/// An `actor`, so its mutable state (including the non-`Sendable` `Process`) is
/// compiler-isolated. Lifecycle is reported via the `onState` callback so an
/// AppModel can reflect it. The supervisor does NOT own the control connection;
/// it only guarantees a running daemon at `socketPath`. A crash-loop (repeated
/// immediate exits) trips a circuit breaker and reports `.failed` rather than
/// restarting forever.
public actor CoreSupervisor {
    public enum State: Equatable, Sendable {
        case stopped
        case starting
        case running(pid: Int32)
        case restarting(afterError: String)
        case failed(reason: String)
    }

    public struct Config: Sendable {
        /// Absolute path to the `xclawd` binary.
        public var binaryPath: String
        /// Unix socket the daemon will listen on (passed as -control).
        public var socketPath: String
        /// SQLite path (-db). Single-bot mode only; defaults to a temp file.
        public var dbPath: String
        /// When non-empty, run in multi-bot config mode: `-config <configPath>
        /// -control <socketPath>`. Empty path with configMode=true uses the
        /// daemon's default ~/.xclaw/config.json. Takes precedence over the
        /// single-bot flags above.
        public var configMode: Bool
        public var configPath: String
        /// Extra args appended verbatim.
        public var extraArgs: [String]
        /// Consecutive immediate failures before the circuit breaker trips and
        /// the supervisor reports `.failed` instead of restarting.
        public var maxConsecutiveFailures: Int
        /// Initial restart backoff (doubles up to `maxBackoff`).
        public var initialBackoff: TimeInterval

        public init(binaryPath: String, socketPath: String, dbPath: String = "",
                    configMode: Bool = false,
                    configPath: String = "", extraArgs: [String] = [],
                    maxConsecutiveFailures: Int = 5,
                    initialBackoff: TimeInterval = 1.0) {
            self.binaryPath = binaryPath
            self.socketPath = socketPath
            self.dbPath = dbPath
            self.configMode = configMode
            self.configPath = configPath
            self.extraArgs = extraArgs
            self.maxConsecutiveFailures = maxConsecutiveFailures
            self.initialBackoff = initialBackoff
        }
    }

    private let config: Config
    private let onState: @Sendable (State) -> Void

    private var process: Process?
    private var stopped = false
    private var backoff: TimeInterval
    private static let maxBackoff: TimeInterval = 30.0
    /// A run shorter than this counts as a failure (crash-loop detection);
    /// a longer run is "healthy" and resets the failure budget.
    private static let healthyUptime: TimeInterval = 15.0
    private var consecutiveFailures = 0
    private var launchedAt: DispatchTime?
    private var restartTask: Task<Void, Never>?

    public init(config: Config, onState: @escaping @Sendable (State) -> Void = { _ in }) {
        self.config = config
        self.onState = onState
        self.backoff = config.initialBackoff
    }

    /// Whether the supervised binary exists and is executable. `nonisolated`:
    /// it only reads the immutable, Sendable `config`.
    public nonisolated var binaryAvailable: Bool {
        FileManager.default.isExecutableFile(atPath: config.binaryPath)
    }

    /// Starts the daemon and keeps it alive until `stop()`.
    public func start() {
        stopped = false
        consecutiveFailures = 0
        backoff = config.initialBackoff
        launch()
    }

    /// Stops the daemon and disables restarts. Waits for the process to actually
    /// exit before returning: the daemon removes its control socket on shutdown
    /// (`defer os.Remove`), so a replacement must not bind the same path until
    /// this one is gone — otherwise the dying daemon's cleanup deletes the new
    /// socket and clients can no longer connect. Escalates to SIGKILL if the
    /// daemon ignores SIGTERM.
    public func stop() async {
        stopped = true
        restartTask?.cancel()
        restartTask = nil
        if let p = process {
            p.terminationHandler = nil
            p.terminate() // SIGTERM
            var waited = 0
            while p.isRunning && waited < 60 { // up to ~3s
                try? await Task.sleep(for: .milliseconds(50))
                waited += 1
            }
            if p.isRunning {
                Log.supervisor.error("xclawd ignored SIGTERM after 3s; sending SIGKILL")
                kill(p.processIdentifier, SIGKILL)
                try? await Task.sleep(for: .milliseconds(100))
            }
        }
        process = nil
        Log.supervisor.info("supervisor stopped")
        onState(.stopped)
    }

    private func launch() {
        if stopped { return }
        guard binaryAvailable else {
            // Treat a missing binary as a retryable error so a later install/
            // build is picked up.
            handleExit(reason: "xclawd not found at \(config.binaryPath)", healthy: false)
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
            args = ["-control", config.socketPath, "-no-repl"]
            if !config.dbPath.isEmpty {
                args += ["-db", config.dbPath]
            }
        }
        // The daemon must not outlive us: exit if this app process dies, even on
        // a crash/force-quit where stop() never runs.
        args.append("-exit-with-parent")
        args += config.extraArgs
        p.arguments = args
        // Inherit stdio so daemon logs surface in the app's console during dev.
        p.standardOutput = FileHandle.standardOutput
        p.standardError = FileHandle.standardError

        p.terminationHandler = { [weak self] proc in
            // Hops back onto the actor; capture only the Sendable status.
            let status = proc.terminationStatus
            Task { await self?.processExited(status: status) }
        }

        do {
            try p.run()
        } catch {
            handleExit(reason: "launch failed: \(error.localizedDescription)", healthy: false)
            return
        }
        process = p
        launchedAt = DispatchTime.now()
        Log.supervisor.info("xclawd launched (pid \(p.processIdentifier))")
        onState(.running(pid: p.processIdentifier))
    }

    private func processExited(status: Int32) {
        if stopped { return }
        let uptime = launchedAt.map {
            Double(DispatchTime.now().uptimeNanoseconds - $0.uptimeNanoseconds) / 1e9
        } ?? 0
        handleExit(reason: "xclawd exited (status \(status))", healthy: uptime >= Self.healthyUptime)
    }

    /// Handles a process exit / launch failure: resets the failure budget if the
    /// previous run was healthy, then either restarts with backoff or trips the
    /// circuit breaker after too many consecutive immediate failures.
    private func handleExit(reason: String, healthy: Bool) {
        if stopped { return }
        process = nil
        if healthy {
            consecutiveFailures = 0
            backoff = config.initialBackoff
        }
        consecutiveFailures += 1

        if consecutiveFailures > config.maxConsecutiveFailures {
            let msg = "xclawd failed \(consecutiveFailures) times in a row; giving up. Last: \(reason)"
            Log.supervisor.fault("\(msg, privacy: .public)")
            onState(.failed(reason: msg))
            return
        }

        let delay = backoff
        backoff = min(backoff * 2, Self.maxBackoff)
        Log.supervisor.error("\(reason, privacy: .public); restarting in \(delay, format: .fixed(precision: 1))s (attempt \(self.consecutiveFailures))")
        onState(.restarting(afterError: reason))
        restartTask = Task { [weak self] in
            try? await Task.sleep(for: .seconds(delay))
            if Task.isCancelled { return }
            await self?.launch()
        }
    }
}
