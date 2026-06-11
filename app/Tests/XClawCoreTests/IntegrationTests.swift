import Testing
import Foundation
@testable import XClawCore

/// End-to-end integration of the AppModel flow, headless: CoreSupervisor spawns
/// the REAL xclawd, ControlClient connects over the bus, a command is sent, and
/// events stream back. Skips automatically if the dev binary isn't built (so it
/// never breaks CI machines without Go).
@Test
func supervisorSpawnsRealCoreAndBusDelivers() async throws {
    guard let bin = devXclawdPath() else {
        // Not built here; this path is exercised by scripts/run-dev.sh.
        return
    }

    let sock = (NSTemporaryDirectory() as NSString)
        .appendingPathComponent("xclaw-it-\(getpid()).sock")
    let db = (NSTemporaryDirectory() as NSString)
        .appendingPathComponent("xclaw-it-\(getpid()).db")
    defer {
        try? FileManager.default.removeItem(atPath: sock)
        try? FileManager.default.removeItem(atPath: db)
    }

    let sup = CoreSupervisor(config: .init(
        binaryPath: bin, socketPath: sock, dbPath: db))
    await sup.start()
    defer { Task { await sup.stop() } }

    // Wait for the daemon to bind the socket.
    var ready = false
    for _ in 0..<50 {
        if FileManager.default.fileExists(atPath: sock) { ready = true; break }
        try await Task.sleep(for: .milliseconds(100))
    }
    #expect(ready, "xclawd did not bind the control socket")
    guard ready else { return }

    let client = ControlClient(path: sock)
    try client.connect()
    defer { client.disconnect() }

    let seen = SeenEvents()
    let consumer = Task.detached {
        for await env in client.events {
            if env.kind == .response { seen.addResponse(env.type) }
            if env.kind == .event { seen.addEvent(env.type) }
        }
    }
    defer { consumer.cancel() }

    _ = try client.send(type: "health", body: [String: String]())
    _ = try client.send(type: "session.send", body: SessionSendBody(uid: "it-user", text: "ping"))

    // Give the turn time to stream back.
    try await Task.sleep(for: .seconds(4))

    #expect(seen.responses.contains("health"), "responses: \(seen.responses)")
    // The turn should at least produce activity events (turnStart/turnDone),
    // even when the agent isn't authenticated (no assistant text).
    #expect(!seen.events.isEmpty, "no events received; events: \(seen.events)")
}

/// Finds the dev-built xclawd (scripts/run-dev.sh writes core/.xclawd-dev).
func devXclawdPath() -> String? {
    let fm = FileManager.default
    if let env = ProcessInfo.processInfo.environment["XCLAWD_BIN"], fm.isExecutableFile(atPath: env) {
        return env
    }
    var dir = URL(fileURLWithPath: fm.currentDirectoryPath)
    for _ in 0..<6 {
        let p = dir.appendingPathComponent("core/.xclawd-dev").path
        if fm.isExecutableFile(atPath: p) { return p }
        dir.deleteLastPathComponent()
    }
    return nil
}

final class SeenEvents: @unchecked Sendable {
    private let lock = NSLock()
    private var ev: [String] = []
    private var resp: [String] = []
    func addEvent(_ t: String) { lock.lock(); ev.append(t); lock.unlock() }
    func addResponse(_ t: String) { lock.lock(); resp.append(t); lock.unlock() }
    var events: [String] { lock.lock(); defer { lock.unlock() }; return ev }
    var responses: [String] { lock.lock(); defer { lock.unlock() }; return resp }
}

/// End-to-end multi-bot: run xclawd in -config mode with a control socket and a
/// 2-bot config, connect the Swift ControlClient, and assert bots.list returns
/// both bots. Skips if the dev binary isn't built.
@Test
func configModeExposesBotsOverBus() async throws {
    guard let bin = devXclawdPath() else { return }

    let tmp = (NSTemporaryDirectory() as NSString)
        .appendingPathComponent("xclaw-mb-\(getpid())")
    let fm = FileManager.default
    try fm.createDirectory(atPath: tmp, withIntermediateDirectories: true)
    defer { try? fm.removeItem(atPath: tmp) }

    let cfgPath = (tmp as NSString).appendingPathComponent("config.json")
    try #"{"apiUrl":"http://127.0.0.1:9","bots":[{"id":"alpha","octoToken":"bf_a"},{"id":"beta","octoToken":"bf_b"}]}"#
        .write(toFile: cfgPath, atomically: true, encoding: .utf8)

    let sock = (tmp as NSString).appendingPathComponent("ctl.sock")

    // Spawn xclawd -config <cfg> -control <sock> directly (config mode isn't a
    // CoreSupervisor scenario; it's the multi-bot daemon entry).
    let proc = Process()
    proc.executableURL = URL(fileURLWithPath: bin)
    proc.arguments = ["-config", cfgPath, "-control", sock]
    proc.standardOutput = FileHandle.nullDevice
    proc.standardError = FileHandle.nullDevice
    try proc.run()
    defer { proc.terminate() }

    // Wait for the socket.
    var ready = false
    for _ in 0..<50 {
        if fm.fileExists(atPath: sock) { ready = true; break }
        try await Task.sleep(for: .milliseconds(100))
    }
    #expect(ready, "control socket not bound")
    guard ready else { return }

    let client = ControlClient(path: sock)
    try client.connect()
    defer { client.disconnect() }

    let gotBots = Box<[String]>([])
    let consumer = Task.detached {
        var state = AppState()
        for await env in client.events where env.kind == .response && env.type == "bots.list" {
            if let infos = env.decodeBody([BotInfo].self) {
                state.setBots(infos)
                gotBots.set(state.sortedBots.map { $0.id })
            }
        }
    }
    defer { consumer.cancel() }

    await pollBots(client, got: gotBots)

    #expect(gotBots.get() == ["alpha", "beta"], "bots.list should expose both bots; got \(gotBots.get())")
}

/// End-to-end through CoreSupervisor in config mode: the supervisor spawns
/// `xclawd -config <path> -control <sock>`, and the bus exposes the configured
/// bots. Proves the supervisor's config-mode arg construction works.
@Test
func supervisorConfigModeRunsBots() async throws {
    guard let bin = devXclawdPath() else { return }
    let fm = FileManager.default

    let tmp = (NSTemporaryDirectory() as NSString)
        .appendingPathComponent("xclaw-sc-\(getpid())")
    try fm.createDirectory(atPath: tmp, withIntermediateDirectories: true)
    defer { try? fm.removeItem(atPath: tmp) }

    let cfgPath = (tmp as NSString).appendingPathComponent("config.json")
    try #"{"apiUrl":"http://127.0.0.1:9","bots":[{"id":"alpha","octoToken":"bf_a"}]}"#
        .write(toFile: cfgPath, atomically: true, encoding: .utf8)

    let sock = (tmp as NSString).appendingPathComponent("ctl.sock")
    let sup = CoreSupervisor(config: .init(
        binaryPath: bin, socketPath: sock, configMode: true, configPath: cfgPath))
    await sup.start()
    defer { Task { await sup.stop() } }

    var ready = false
    for _ in 0..<50 {
        if fm.fileExists(atPath: sock) { ready = true; break }
        try await Task.sleep(for: .milliseconds(100))
    }
    #expect(ready, "supervisor did not bring up the control socket in config mode")
    guard ready else { return }

    let client = ControlClient(path: sock)
    try client.connect()
    defer { client.disconnect() }

    let gotBots = Box<[String]>([])
    let consumer = Task.detached {
        var state = AppState()
        for await env in client.events where env.kind == .response && env.type == "bots.list" {
            if let infos = env.decodeBody([BotInfo].self) {
                state.setBots(infos)
                gotBots.set(state.sortedBots.map { $0.id })
            }
        }
    }
    defer { consumer.cancel() }

    await pollBots(client, got: gotBots)
    #expect(gotBots.get() == ["alpha"], "supervisor config mode should expose the configured bot; got \(gotBots.get())")
}

final class Box<T>: @unchecked Sendable {
    private let lock = NSLock()
    private var v: T
    init(_ v: T) { self.v = v }
    func set(_ nv: T) { lock.lock(); v = nv; lock.unlock() }
    func get() -> T { lock.lock(); defer { lock.unlock() }; return v }
}

/// Polls bots.list over the client until the collected ids are non-empty or the
/// deadline passes. Robust against scheduling jitter when several live tests
/// spawn daemons in parallel (a single send + fixed sleep was flaky).
func pollBots(_ client: ControlClient, got: Box<[String]>, attempts: Int = 20) async {
    for _ in 0..<attempts {
        _ = try? client.send(type: "bots.list", body: [String: String]())
        try? await Task.sleep(for: .milliseconds(250))
        if !got.get().isEmpty { return }
    }
}

