import Foundation

/// Resolves where the `xclawd` binary and control socket live across dev and
/// bundled (.app) layouts.
public enum CorePaths {
    /// Per-user XClaw support directory (~/Library/Application Support/XClaw).
    public static var supportDir: URL {
        let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first
            ?? URL(fileURLWithPath: NSHomeDirectory()).appendingPathComponent("Library/Application Support")
        return base.appendingPathComponent("XClaw", isDirectory: true)
    }

    /// Control socket path. Kept short (sockaddr_un caps the path length).
    public static var socketPath: String {
        // Use a stable per-uid path under the temp dir to stay well under the
        // ~104-byte sun_path limit.
        let uid = getuid()
        return (NSTemporaryDirectory() as NSString).appendingPathComponent("xclaw-\(uid).sock")
    }

    /// Default SQLite path under the support dir.
    public static var dbPath: String {
        supportDir.appendingPathComponent("xclaw.db").path
    }

    /// The daemon's bot-first config: ~/.xclaw/config.json (matches the Go
    /// core's config.DefaultConfigPath — NOT the app support dir).
    public static var configPath: String {
        URL(fileURLWithPath: NSHomeDirectory())
            .appendingPathComponent(".xclaw/config.json").path
    }

    /// Whether a daemon config file exists (selects multi-bot config mode).
    public static var configExists: Bool {
        FileManager.default.fileExists(atPath: configPath)
    }

    /// Locates the xclawd binary, trying, in order:
    ///  1. $XCLAWD_BIN (explicit override, used by dev scripts)
    ///  2. the app bundle's Contents/Helpers/xclawd (production)
    ///  3. the monorepo dev build at ../core (when run via `swift run`)
    public static func resolveBinary() -> String? {
        let fm = FileManager.default

        if let override = ProcessInfo.processInfo.environment["XCLAWD_BIN"],
           fm.isExecutableFile(atPath: override) {
            return override
        }

        if let helpers = Bundle.main.url(forResource: nil, withExtension: nil,
                                         subdirectory: "Helpers") {
            let candidate = helpers.appendingPathComponent("xclawd").path
            if fm.isExecutableFile(atPath: candidate) {
                return candidate
            }
        }
        // Bundle.main.executableURL/../../Helpers/xclawd (Contents/Helpers).
        if let exe = Bundle.main.executableURL {
            let bundled = exe.deletingLastPathComponent()
                .appendingPathComponent("../Helpers/xclawd").standardized.path
            if fm.isExecutableFile(atPath: bundled) {
                return bundled
            }
        }

        // Dev: monorepo layout app/ + core/. Walk up from CWD to find core/.
        var dir = URL(fileURLWithPath: fm.currentDirectoryPath)
        for _ in 0..<5 {
            let devBin = dir.appendingPathComponent("core/.xclawd-dev").path
            if fm.isExecutableFile(atPath: devBin) {
                return devBin
            }
            dir.deleteLastPathComponent()
        }
        return nil
    }

    /// Ensures the support dir exists.
    public static func ensureSupportDir() {
        try? FileManager.default.createDirectory(at: supportDir, withIntermediateDirectories: true)
    }
}
