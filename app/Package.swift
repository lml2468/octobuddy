// swift-tools-version: 6.0
import PackageDescription

// XClaw macOS app — native control plane / GUI for the xclawd Go core.
// Three targets: XClawCore (control-bus client + AppState reducer + config/
// Keychain, agent/IM-agnostic), XClawApp (SwiftUI + AppKit shell), and
// XClawProbe (a CLI harness that drives a running control socket end-to-end).
let package = Package(
    name: "XClaw",
    defaultLocalization: "en",
    platforms: [
        .macOS(.v14),
    ],
    products: [
        .executable(name: "XClawApp", targets: ["XClawApp"]),
        .executable(name: "xclaw-probe", targets: ["XClawProbe"]),
        .library(name: "XClawCore", targets: ["XClawCore"]),
    ],
    targets: [
        // Models + control-bus client (NDJSON over UDS) + AppState reducer.
        // Mirrors Open Island's OpenIslandCore: the IM/agent-agnostic core.
        .target(
            name: "XClawCore"
        ),
        // SwiftUI + AppKit shell. Owns AppModel, talks to XClawCore.
        // Mirrors Open Island's OpenIslandApp.
        .executableTarget(
            name: "XClawApp",
            dependencies: ["XClawCore"],
            // The custom-symbol asset catalog is compiled by actool in
            // scripts/package-app.sh (command-line SwiftPM can't run actool), so
            // exclude it here to avoid an unhandled-files warning.
            exclude: ["Resources/Symbols.xcassets"]
        ),
        // CLI harness: connect to a running xclawd control socket, send a
        // command, print the event stream. Used to prove Swift↔Go end-to-end.
        .executableTarget(
            name: "XClawProbe",
            dependencies: ["XClawCore"]
        ),
        .testTarget(
            name: "XClawCoreTests",
            dependencies: ["XClawCore"]
        ),
    ]
)
