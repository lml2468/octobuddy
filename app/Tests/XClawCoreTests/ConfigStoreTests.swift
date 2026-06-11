import Testing
import Foundation
@testable import XClawCore

private func withTempBase(_ body: (URL) throws -> Void) rethrows {
    let dir = URL(fileURLWithPath: NSTemporaryDirectory())
        .appendingPathComponent("xclaw-cfg-\(UUID().uuidString)")
    defer { try? FileManager.default.removeItem(at: dir) }
    try body(dir)
}

@Test
func configSaveStripsTokensFromFile() throws {
    try withTempBase { base in
        let bots = [
            BotConfig(id: "alpha", apiURL: "https://octo.example", octoToken: "bf_a"),
            BotConfig(id: "beta", apiURL: "https://octo.example", octoToken: "bf_b"),
        ]
        try ConfigStore.save(bots, base: base)
        // Non-secret fields persist; tokens are NOT written to the file.
        let loaded = try ConfigStore.load(base: base)
        #expect(loaded.count == 2)
        #expect(loaded.allSatisfy { $0.octoToken.isEmpty })
        #expect(loaded.first { $0.id == "beta" }?.apiURL == "https://octo.example")
        // And not present in the raw JSON either.
        let raw = try String(contentsOf: base.appendingPathComponent("config.json"), encoding: .utf8)
        #expect(!raw.contains("bf_a") && !raw.contains("bf_b"))
        #expect(!raw.contains("octoToken"))
    }
}

@Test
func configGatewayAndEnvRoundTrip() throws {
    try withTempBase { base in
        let bot = BotConfig(
            id: "alpha", apiURL: "https://octo.example", octoToken: "bf_a",
            gatewayBaseURL: "https://gw.example/v1", gatewayToken: "sk-tok",
            env: ["OCTO_BOT_ID": "alpha", "GH_TOKEN": "ghp_x"])
        try ConfigStore.save([bot], base: base)
        let loaded = try ConfigStore.load(base: base)
        let a = loaded.first { $0.id == "alpha" }
        // Non-secret gateway/env settings persist; the gateway token does not.
        #expect(a?.gatewayBaseURL == "https://gw.example/v1")
        #expect(a?.gatewayToken.isEmpty == true)
        #expect(a?.env["OCTO_BOT_ID"] == "alpha")
        #expect(a?.env["GH_TOKEN"] == "ghp_x")
        let raw = try String(contentsOf: base.appendingPathComponent("config.json"), encoding: .utf8)
        #expect(!raw.contains("sk-tok") && !raw.contains("gatewayToken"))
    }
}

@Test
func configRemoveBotPrunesExplicitly() throws {
    try withTempBase { base in
        let fm = FileManager.default
        try ConfigStore.save([
            BotConfig(id: "keep", apiURL: "https://o", octoToken: "t1"),
            BotConfig(id: "drop", apiURL: "https://o", octoToken: "t2"),
        ], base: base)
        // Simulate each bot having a runtime data dir (what the daemon creates).
        for id in ["keep", "drop"] {
            try fm.createDirectory(at: base.appendingPathComponent("\(id)/data"),
                                   withIntermediateDirectories: true)
        }

        // Saving without "drop" but WITHOUT naming it in `removing` must NOT
        // delete its data — pruning is explicit-only, so a partial/failed load
        // can never wipe a bot's history.
        try ConfigStore.save([BotConfig(id: "keep", apiURL: "https://o", octoToken: "t1")], base: base)
        #expect(fm.fileExists(atPath: base.appendingPathComponent("drop/data").path))

        // Explicitly removing "drop" prunes its subtree; "keep" is untouched.
        try ConfigStore.save([BotConfig(id: "keep", apiURL: "https://o", octoToken: "t1")],
                             base: base, removing: ["drop"])
        #expect(!fm.fileExists(atPath: base.appendingPathComponent("drop").path))
        #expect(fm.fileExists(atPath: base.appendingPathComponent("keep/data").path))
        #expect(try ConfigStore.load(base: base).count == 1)
    }
}

@Test
func configSaveNeverPrunesLiveBot() throws {
    try withTempBase { base in
        let fm = FileManager.default
        try ConfigStore.save([BotConfig(id: "keep", apiURL: "https://o")], base: base)
        try fm.createDirectory(at: base.appendingPathComponent("keep/data"),
                               withIntermediateDirectories: true)
        // Even if "keep" is in `removing`, a still-present (re-added) bot is never
        // pruned.
        try ConfigStore.save([BotConfig(id: "keep", apiURL: "https://o")],
                             base: base, removing: ["keep"])
        #expect(fm.fileExists(atPath: base.appendingPathComponent("keep/data").path))
    }
}

@Test
func configRejectsInvalidSlug() throws {
    try withTempBase { base in
        #expect(throws: ConfigStore.ConfigError.self) {
            try ConfigStore.save([BotConfig(id: "../escape", octoToken: "t")], base: base)
        }
    }
}

@Test
func configRejectsDuplicateID() throws {
    try withTempBase { base in
        #expect(throws: ConfigStore.ConfigError.self) {
            try ConfigStore.save([
                BotConfig(id: "same", octoToken: "t1"),
                BotConfig(id: "same", octoToken: "t2"),
            ], base: base)
        }
    }
}

@Test
func slugValidation() {
    #expect(ConfigStore.validSlug("alpha-1.bot_2"))
    #expect(!ConfigStore.validSlug(""))
    #expect(!ConfigStore.validSlug("."))
    #expect(!ConfigStore.validSlug(".."))
    #expect(!ConfigStore.validSlug("a/b"))
    #expect(!ConfigStore.validSlug("a b"))
}

@Test
func configPreservesModelAndUnmanagedKeys() throws {
    try withTempBase { base in
        try FileManager.default.createDirectory(at: base, withIntermediateDirectories: true)
        // A config with keys the editor doesn't manage (rateLimit/context) + model.
        let raw = #"{"apiUrl":"https://o","rateLimit":{"maxPerMinute":7},"context":{"maxContextChars":9000},"bots":[{"id":"alpha","apiUrl":"https://o","agent":{"model":"claude-opus-4-8","env":{"K":"V"}},"context":{"maxContextChars":1234}}]}"#
        try raw.data(using: .utf8)!.write(to: base.appendingPathComponent("config.json"))

        var bots = try ConfigStore.load(base: base)
        #expect(bots.first?.model == "claude-opus-4-8")
        #expect(bots.first?.env["K"] == "V")
        bots[0].apiURL = "https://o2"     // edit one managed field
        try ConfigStore.save(bots, base: base)

        let data = try Data(contentsOf: base.appendingPathComponent("config.json"))
        let root = try JSONSerialization.jsonObject(with: data) as! [String: Any]
        // top-level + per-bot unmanaged keys preserved
        #expect((root["rateLimit"] as? [String: Any])?["maxPerMinute"] as? Int == 7)
        #expect((root["context"] as? [String: Any])?["maxContextChars"] as? Int == 9000)
        let b0 = (root["bots"] as! [[String: Any]])[0]
        #expect((b0["agent"] as? [String: Any])?["model"] as? String == "claude-opus-4-8")
        #expect((b0["context"] as? [String: Any])?["maxContextChars"] as? Int == 1234)
        #expect(b0["apiUrl"] as? String == "https://o2")
    }
}

@Test
func configPersonaRoundTrip() throws {
    try withTempBase { base in
        let fm = FileManager.default
        var bot = BotConfig(id: "alpha", apiURL: "https://o")
        bot.soul = "You are Alpha, a terse ops bot."
        bot.agents = "Always confirm destructive actions."
        try ConfigStore.save([bot], base: base)

        #expect(fm.fileExists(atPath: base.appendingPathComponent("alpha/SOUL.md").path))
        #expect(fm.fileExists(atPath: base.appendingPathComponent("alpha/AGENTS.md").path))

        let loaded = try ConfigStore.load(base: base).first
        #expect(loaded?.soul == "You are Alpha, a terse ops bot.")
        #expect(loaded?.agents == "Always confirm destructive actions.")

        // Clearing a prompt removes its file (Go treats absent as omitted).
        var cleared = loaded!
        cleared.soul = ""
        try ConfigStore.save([cleared], base: base)
        #expect(!fm.fileExists(atPath: base.appendingPathComponent("alpha/SOUL.md").path))
        #expect(fm.fileExists(atPath: base.appendingPathComponent("alpha/AGENTS.md").path))
    }
}

/// Interop: a config written by the Swift ConfigStore must be parseable by the
/// Go core (config.Load). Runs the dev xclawd with -config pointed at the
/// Swift-written dir and asserts bots.list returns the bots. Skips if the dev
/// binary isn't built.
@Test
func swiftWrittenConfigParsesInGo() async throws {
    guard let bin = devXclawdPath() else { return }
    let dir = URL(fileURLWithPath: NSTemporaryDirectory())
        .appendingPathComponent("xclaw-interop-\(getpid())")
    defer { try? FileManager.default.removeItem(at: dir) }

    try ConfigStore.save([
        BotConfig(id: "alpha", apiURL: "http://127.0.0.1:9", octoToken: "bf_a"),
        BotConfig(id: "beta", apiURL: "http://127.0.0.1:9", octoToken: "bf_b"),
    ], base: dir)

    let sock = dir.appendingPathComponent("ctl.sock").path
    let proc = Process()
    proc.executableURL = URL(fileURLWithPath: bin)
    proc.arguments = ["-config", dir.appendingPathComponent("config.json").path, "-control", sock]
    proc.standardOutput = FileHandle.nullDevice
    proc.standardError = FileHandle.nullDevice
    try proc.run()
    defer { proc.terminate() }

    var ready = false
    for _ in 0..<50 {
        if FileManager.default.fileExists(atPath: sock) { ready = true; break }
        try await Task.sleep(for: .milliseconds(100))
    }
    #expect(ready, "Go core didn't start with the Swift-written config")
    guard ready else { return }

    let client = ControlClient(path: sock)
    try client.connect()
    defer { client.disconnect() }

    let got = Box<[String]>([])
    let consumer = Task.detached {
        var state = AppState()
        for await env in client.events where env.kind == .response && env.type == "bots.list" {
            if let infos = env.decodeBody([BotInfo].self) {
                state.setBots(infos)
                got.set(state.sortedBots.map { $0.id })
            }
        }
    }
    defer { consumer.cancel() }

    await pollBots(client, got: got)
    #expect(got.get() == ["alpha", "beta"], "Go didn't parse the Swift config; got \(got.get())")
}
