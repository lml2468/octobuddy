import Foundation

/// Read/write the daemon's single ~/.xclaw/config.json from the GUI, producing
/// JSON the Go `config.Load` parses. Every bot is inlined in the global config's
/// bots[] array (id + apiUrl + agent overrides). Per-bot persona/behavior lives
/// in SOUL.md / AGENTS.md under ~/.xclaw/<id>/ and is read/written here too.
///
/// Secrets are NOT written by `save` — `octoToken` / `gatewayToken` live in the
/// Keychain (see Keychain.swift) and are injected at runtime. `load` still reads
/// any tokens present in the file (legacy / headless), so callers can migrate
/// them into the Keychain. `save` MERGES into the existing config.json, so keys
/// the editor doesn't manage (rateLimit, context, top-level agent defaults) are
/// preserved rather than dropped.
public struct BotConfig: Sendable, Equatable, Identifiable {
    /// Stable per-instance identity for SwiftUI selection/ForEach. Distinct from
    /// `id` (the bot slug, which the user can edit) and not persisted, so editing
    /// the slug never changes a row's identity.
    public let rowID = UUID()
    public var id: String
    public var apiURL: String
    public var octoToken: String
    /// Model the agent should use (Go: agent.model), e.g. "claude-opus-4-8".
    public var model: String
    /// Model-gateway routing. The Go core maps these to the claude env var
    /// names (ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN).
    public var gatewayBaseURL: String
    public var gatewayToken: String
    /// Arbitrary extra environment variables injected into the agent CLI
    /// (e.g. OCTO_BOT_ID, GH_TOKEN, GLAB_TOKEN).
    public var env: [String: String]
    /// Persona (SOUL.md) and behavior norms (AGENTS.md) — separate files under
    /// ~/.xclaw/<id>/, not part of config.json.
    public var soul: String
    public var agents: String

    public init(id: String, apiURL: String = "",
                octoToken: String = "", model: String = "",
                gatewayBaseURL: String = "", gatewayToken: String = "",
                env: [String: String] = [:], soul: String = "", agents: String = "") {
        self.id = id
        self.apiURL = apiURL
        self.octoToken = octoToken
        self.model = model
        self.gatewayBaseURL = gatewayBaseURL
        self.gatewayToken = gatewayToken
        self.env = env
        self.soul = soul
        self.agents = agents
    }
}

public enum ConfigStore {
    public enum ConfigError: Error, CustomStringConvertible {
        case invalidSlug(String)
        case duplicateID(String)
        case io(String)
        public var description: String {
            switch self {
            case .invalidSlug(let s): return "invalid bot id \"\(s)\": use letters, digits, dot, underscore, hyphen (no path separators)"
            case .duplicateID(let s): return "duplicate bot id \"\(s)\""
            case .io(let s): return s
            }
        }
    }

    /// Default ~/.xclaw directory.
    public static var baseDir: URL {
        URL(fileURLWithPath: NSHomeDirectory()).appendingPathComponent(".xclaw", isDirectory: true)
    }
    static func globalConfigURL(_ base: URL) -> URL { base.appendingPathComponent("config.json") }

    // Matches the Go slug rule: ^[A-Za-z0-9._-]+$, not "." or "..".
    public static func validSlug(_ id: String) -> Bool {
        guard !id.isEmpty, id != ".", id != ".." else { return false }
        return id.allSatisfy { c in
            c.isLetter && c.isASCII || c.isNumber && c.isASCII || c == "." || c == "_" || c == "-"
        }
    }

    // MARK: - load

    /// Loads the configured bots from `base` (default ~/.xclaw)'s single
    /// config.json, plus each bot's SOUL.md / AGENTS.md. Returns [] if no config.
    public static func load(base: URL? = nil) throws -> [BotConfig] {
        let base = base ?? baseDir
        guard let data = try? Data(contentsOf: globalConfigURL(base)),
              let root = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] else {
            return []
        }
        let topAPI = root["apiUrl"] as? String
        let bots = (root["bots"] as? [[String: Any]]) ?? []
        return bots.compactMap { b in
            guard let id = b["id"] as? String else { return nil }
            let agent = b["agent"] as? [String: Any]
            let (soul, agents) = prompts(base: base, id: id)
            return BotConfig(
                id: id,
                apiURL: (b["apiUrl"] as? String) ?? topAPI ?? "",
                octoToken: (b["octoToken"] as? String) ?? "",
                model: (agent?["model"] as? String) ?? "",
                gatewayBaseURL: (agent?["gatewayBaseUrl"] as? String) ?? "",
                gatewayToken: (agent?["gatewayToken"] as? String) ?? "",
                env: (agent?["env"] as? [String: String]) ?? [:],
                soul: soul,
                agents: agents)
        }
    }

    // MARK: - save

    /// Persists the bot list. MERGES into the existing config.json so unmanaged
    /// keys (rateLimit, context, top-level agent defaults, unknown per-bot keys)
    /// are preserved. Secrets are stripped (octoToken / agent.gatewayToken →
    /// Keychain). Also writes each bot's SOUL.md / AGENTS.md.
    ///
    /// `removing` lists bot ids the caller EXPLICITLY removed; only those
    /// per-bot directories are deleted. Pruning is never inferred from a
    /// set-difference against the on-disk dirs: a failed/partial load would then
    /// look like "every other bot was removed" and wipe their data. Explicit-only
    /// pruning means a save can never destroy data for a bot the caller didn't
    /// knowingly remove.
    public static func save(_ bots: [BotConfig], base: URL? = nil, removing: [String] = []) throws {
        let base = base ?? baseDir
        var seen = Set<String>()
        for b in bots {
            guard validSlug(b.id) else { throw ConfigError.invalidSlug(b.id) }
            guard seen.insert(b.id).inserted else { throw ConfigError.duplicateID(b.id) }
        }

        try mkdir(base)
        let url = globalConfigURL(base)

        // Start from existing JSON to preserve keys we don't manage.
        var root: [String: Any] = {
            if let data = try? Data(contentsOf: url),
               let obj = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] {
                return obj
            }
            return [:]
        }()
        var existing: [String: [String: Any]] = [:]
        for b in (root["bots"] as? [[String: Any]]) ?? [] {
            if let id = b["id"] as? String { existing[id] = b }
        }

        root["bots"] = bots.map { b -> [String: Any] in
            var dict = existing[b.id] ?? [:]
            dict["id"] = b.id
            setOrRemove(&dict, "apiUrl", b.apiURL)
            dict["octoToken"] = nil // secret → Keychain

            var agent = dict["agent"] as? [String: Any] ?? [:]
            setOrRemove(&agent, "model", b.model)
            setOrRemove(&agent, "gatewayBaseUrl", b.gatewayBaseURL)
            agent["gatewayToken"] = nil // secret → Keychain
            if b.env.isEmpty { agent["env"] = nil } else { agent["env"] = b.env }
            dict["agent"] = agent.isEmpty ? nil : agent
            return dict
        }
        // A top-level apiUrl default helps single-bot configs; preserve if set.
        if root["apiUrl"] == nil, let first = bots.first?.apiURL, !first.isEmpty {
            root["apiUrl"] = first
        }

        do {
            let data = try JSONSerialization.data(withJSONObject: root,
                                                  options: [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes])
            try data.write(to: url, options: .atomic)
        } catch {
            throw ConfigError.io("write \(url.path): \(error.localizedDescription)")
        }

        // Persona files per bot.
        for b in bots { try savePrompts(base: base, id: b.id, soul: b.soul, agents: b.agents) }

        // Prune ONLY the per-bot dirs the caller explicitly asked to remove, and
        // never one that's still live (re-added under the same id). Touch only
        // dirs that look like ours (valid slug + a data/ child).
        let live = Set(bots.map { $0.id })
        let fm = FileManager.default
        for id in removing {
            guard validSlug(id), !live.contains(id) else { continue }
            let dir = base.appendingPathComponent(id, isDirectory: true)
            if fm.fileExists(atPath: dir.appendingPathComponent("data").path) {
                try? fm.removeItem(at: dir)
            }
        }
    }

    // MARK: - persona files (SOUL.md / AGENTS.md under <base>/<id>/)

    private static func prompts(base: URL, id: String) -> (soul: String, agents: String) {
        let dir = base.appendingPathComponent(id, isDirectory: true)
        let soul = (try? String(contentsOf: dir.appendingPathComponent("SOUL.md"), encoding: .utf8)) ?? ""
        let agents = (try? String(contentsOf: dir.appendingPathComponent("AGENTS.md"), encoding: .utf8)) ?? ""
        return (soul, agents)
    }

    private static func savePrompts(base: URL, id: String, soul: String, agents: String) throws {
        let dir = base.appendingPathComponent(id, isDirectory: true)
        try writePrompt(dir, "SOUL.md", soul)
        try writePrompt(dir, "AGENTS.md", agents)
    }

    private static func writePrompt(_ dir: URL, _ name: String, _ content: String) throws {
        let url = dir.appendingPathComponent(name)
        let fm = FileManager.default
        let trimmed = content.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty {
            try? fm.removeItem(at: url) // empty → no file (Go treats it as absent)
            return
        }
        try mkdir(dir)
        do { try content.data(using: .utf8)?.write(to: url, options: .atomic) }
        catch { throw ConfigError.io("write \(url.path): \(error.localizedDescription)") }
    }

    // MARK: - helpers

    /// Sets `dict[key] = value`, or removes the key when value is empty.
    private static func setOrRemove(_ dict: inout [String: Any], _ key: String, _ value: String) {
        if value.isEmpty { dict[key] = nil } else { dict[key] = value }
    }

    private static func mkdir(_ url: URL) throws {
        do { try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true) }
        catch { throw ConfigError.io("mkdir \(url.path): \(error.localizedDescription)") }
    }
}
