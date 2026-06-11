import Foundation
import Observation
import XClawCore

/// View model for the bot-configuration editor (its own window, opened via ⌘,).
/// Owns the editable bot list and persists it: non-secret fields to ~/.xclaw/config.json
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
    /// True once the editor has successfully loaded the on-disk config. Guards
    /// loadIfNeeded so re-opening the window doesn't clobber unsaved edits.
    private var hasLoaded = false

    /// Loads the config once (on first window appear). Subsequent appears are a
    /// no-op so a user's unsaved edits survive closing/reopening the window. A
    /// failed load is retried on the next appear.
    func loadIfNeeded() {
        guard !hasLoaded else { return }
        load()
        if error == nil { hasLoaded = true }
    }

    /// Loads the on-disk bot configs, overlaying each bot's tokens from the
    /// Keychain (falling back to any legacy value still in the file). Replaces
    /// the in-memory list — call loadIfNeeded() from the UI to avoid clobbering
    /// unsaved edits.
    func load() {
        error = nil
#if DEBUG
        // UI-preview mode renders seeded sample bots (see AppModel); skip the
        // real config/Keychain read so screenshots need no daemon or Keychain.
        if ProcessInfo.processInfo.environment["XCLAW_UI_PREVIEW"] != nil {
            hasLoaded = true
            return
        }
#endif
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

    /// Removes a bot from the editable list (not yet saved), by stable rowID.
    /// Records its slug so the next successful save prunes that bot's on-disk
    /// dir — pruning is driven by explicit removals, never inferred.
    func remove(rowID: UUID) {
        if let b = bots.first(where: { $0.rowID == rowID }) {
            removedSlugs.insert(b.id)
        }
        bots.removeAll { $0.rowID == rowID }
    }

    /// Slugs the user explicitly removed since the last successful save.
    private var removedSlugs: Set<String> = []

    /// Validates and writes the editable config: non-secret fields to the file,
    /// tokens to the Keychain (empty value deletes the item). Sets needsRestart
    /// on success; returns true on success.
    @discardableResult
    func save() -> Bool {
        error = nil
        do {
            // Prune only bots the user removed that aren't present again.
            let live = Set(bots.map { $0.id })
            let prune = removedSlugs.subtracting(live)
            try ConfigStore.save(bots, removing: Array(prune)) // strips tokens from the file
            removedSlugs.removeAll()
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
