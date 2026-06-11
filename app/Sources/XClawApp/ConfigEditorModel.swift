import Foundation
import Observation
import XClawCore

/// View model for the bot-configuration editor (Settings / Cmd-,). Owns the
/// editable bot list and persists it: non-secret fields to ~/.xclaw/config.json
/// (via `ConfigStore`), tokens to the Keychain. Kept separate from `AppModel` so
/// runtime connection/supervision and config editing are distinct concerns.
@MainActor
@Observable
final class ConfigEditorModel {
    /// The editable bot list (tokens overlaid from the Keychain on load).
    var bots: [BotConfig] = []
    /// Last error from load/save, surfaced in the editor footer.
    var error: String?
    /// Set after a successful save so the UI can prompt for a core restart.
    var needsRestart = false

    /// Loads the on-disk bot configs, overlaying each bot's tokens from the
    /// Keychain (falling back to any legacy value still in the file).
    func load() {
        error = nil
        do {
            var loaded = try ConfigStore.load()
            for i in loaded.indices {
                if let t = Keychain.get(account: Keychain.account(bot: loaded[i].id, kind: Keychain.kindOcto)) {
                    loaded[i].octoToken = t
                }
                if let t = Keychain.get(account: Keychain.account(bot: loaded[i].id, kind: Keychain.kindGateway)) {
                    loaded[i].gatewayToken = t
                }
            }
            bots = loaded
        } catch {
            self.error = "\(error)"
        }
    }

    /// Adds a new bot to the editable list (not yet saved).
    func add() {
        let base = "bot"
        var n = bots.count + 1
        var id = "\(base)\(n)"
        let existing = Set(bots.map { $0.id })
        while existing.contains(id) { n += 1; id = "\(base)\(n)" }
        // Inherit apiUrl from an existing bot for convenience.
        bots.append(BotConfig(id: id, apiURL: bots.first?.apiURL ?? ""))
    }

    /// Removes a bot from the editable list (not yet saved).
    func remove(_ id: String) {
        bots.removeAll { $0.id == id }
    }

    /// Validates and writes the editable config: non-secret fields to the file,
    /// tokens to the Keychain (empty value deletes the item). Sets needsRestart
    /// on success; returns true on success.
    @discardableResult
    func save() -> Bool {
        error = nil
        do {
            try ConfigStore.save(bots) // strips tokens from the file
            for b in bots {
                try Keychain.set(account: Keychain.account(bot: b.id, kind: Keychain.kindOcto), value: b.octoToken)
                try Keychain.set(account: Keychain.account(bot: b.id, kind: Keychain.kindGateway), value: b.gatewayToken)
            }
            needsRestart = true
            return true
        } catch {
            self.error = "\(error)"
            return false
        }
    }

    /// Moves any plaintext tokens left in config.json into the Keychain, then
    /// rewrites the file without them. No-op once the file is clean. Run at
    /// launch before the core starts.
    func migrateLegacyTokens() {
        guard let loaded = try? ConfigStore.load() else { return }
        var migrated = false
        do {
            for b in loaded {
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
                try ConfigStore.save(loaded) // save() strips tokens from the file
                Log.keychain.notice("migrated plaintext token(s) from config.json into the Keychain")
            }
        } catch {
            // Leave the file as-is if migration fails; tokens still work as a
            // plaintext fallback. Surface why so it's diagnosable.
            Log.keychain.error("token migration failed: \(error.localizedDescription, privacy: .public)")
        }
    }
}
