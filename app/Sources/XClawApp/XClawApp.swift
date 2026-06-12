import SwiftUI
import AppKit
import XClawCore

@main
struct XClawApp: App {
    @State private var model = AppModel()
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate

    /// Forces a color scheme for UI preview screenshots (XCLAW_UI_PREVIEW=dark);
    /// nil in normal use → follows the system appearance.
    static var previewScheme: ColorScheme? {
        ProcessInfo.processInfo.environment["XCLAW_UI_PREVIEW"] == "dark" ? .dark : nil
    }

    init() {
        // Start the core on launch — a menu-bar (LSUIElement) app may never open
        // a window, so we can't rely on a view's onAppear to boot the daemon.
        let m = model
        Task { @MainActor in m.start() }
    }

    var body: some Scene {
        // Menu bar presence: a compact status popover with quick actions.
        MenuBarExtra {
            MenuBarContent(model: model)
        } label: {
            MenuBarLabel(model: model)
        }
        .menuBarExtraStyle(.window)

        Window("XClaw", id: "console") {
            ConsoleView(model: model)
                .onAppear { if model.coreState == .stopped { model.start() } }
                .tint(.brand)
                .background(WindowAccessor())
                .preferredColorScheme(Self.previewScheme)
        }
        .defaultSize(width: 1200, height: 780)
        .windowToolbarStyle(.unified)
        .windowStyle(.hiddenTitleBar)
        .windowResizability(.contentMinSize)

        // Bot configuration editor. A real Window, NOT a Settings pane: a
        // master/detail NavigationSplitView needs a split-view window to render
        // a flush, full-height sidebar — inside a Settings scene it collapses to
        // a floating inset card with a dead top gap. Opened via ⌘, (the
        // .appSettings command below) and the menu-bar "Edit Bots…" item.
        Window("Edit Bots", id: "bot-editor") {
            ConfigEditorView(config: model.config,
                             onSaveAndRestart: { model.applyConfigAndRestart() })
                .onAppear { model.config.loadIfNeeded() }
                .tint(.brand)
                .background(WindowAccessor())
                .preferredColorScheme(Self.previewScheme)
        }
        .defaultSize(width: 1000, height: 720)
        .windowToolbarStyle(.unified)
        .windowStyle(.hiddenTitleBar)
        .windowResizability(.contentMinSize)
        .commands {
            // XClaw's "settings" IS the bot editor: ⌘, opens it (replacing the
            // default, now-empty "Settings…" app-menu item).
            CommandGroup(replacing: .appSettings) { EditBotsCommand() }
        }
    }
}

// MARK: - Menu bar popover

/// The ⌘, command that opens the bot editor window. XClaw has no traditional
/// preferences pane — its "settings" is the bot editor — so this replaces the
/// default "Settings…" app-menu item.
private struct EditBotsCommand: View {
    @Environment(\.openWindow) private var openWindow
    var body: some View {
        Button("Edit Bots…") {
            NSApp.activate(ignoringOtherApps: true)
            openWindow(id: "bot-editor")
        }
        .keyboardShortcut(",", modifiers: .command)
    }
}

/// The menu-bar status icon — the XClaw octopus as a custom SF Symbol compiled
/// into the app bundle's asset catalog (see scripts/package-app.sh), so the menu
/// bar auto-tints it exactly like a built-in symbol. Falls back to a bolt SF
/// Symbol when the custom symbol isn't present (e.g. plain `swift build` dev
/// runs, where no Assets.car is produced).
private struct MenuBarLabel: View {
    @Bindable var model: AppModel
    private static let hasOctopus = NSImage(named: "octopus") != nil
    var body: some View {
        Group {
            if Self.hasOctopus {
                Image("octopus")   // custom symbol from the bundle's Assets.car
                    .font(.system(size: 22)).imageScale(.large)
            } else {
                Image(systemName: model.connected ? "bolt.horizontal.circle.fill" : "bolt.horizontal.circle")
                    .font(.system(size: 16))
            }
        }
        .accessibilityLabel(model.connected ? "XClaw, connected" : "XClaw, disconnected")
    }
}

/// Menu-bar dropdown — a small status card with quick actions. Uses
/// `@Environment(\.openWindow)` to surface the console window.
private struct MenuBarContent: View {
    @Bindable var model: AppModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 10) {
                OctopusShape()
                    .fill(style: FillStyle(eoFill: true))
                    .foregroundStyle(model.connected ? Color.brand : Color.secondary)
                    .frame(width: 26, height: 26)
                    .accessibilityHidden(true)
                VStack(alignment: .leading, spacing: 1) {
                    Text("XClaw").appFont(.headline)
                    Text(statusLine).appFont(.caption).foregroundStyle(.secondary)
                }
                Spacer()
            }
            .padding(.horizontal, 14)
            .padding(.vertical, 12)

            Divider().opacity(0.5)

            VStack(spacing: 2) {
                MenuRow(title: "Open Console", systemImage: "macwindow") {
                    NSApp.activate(ignoringOtherApps: true)
                    openWindow(id: "console")
                }
                MenuRow(title: "Edit Bots…", systemImage: "slider.horizontal.3") {
                    NSApp.activate(ignoringOtherApps: true)
                    openWindow(id: "bot-editor")
                }
                MenuRow(title: "Restart Core", systemImage: "arrow.clockwise") {
                    model.restartCore()
                }
                Divider().opacity(0.5).padding(.vertical, 4)
                MenuRow(title: "Quit XClaw", systemImage: "power") {
                    model.stop(); NSApplication.shared.terminate(nil)
                }
            }
            .padding(8)
        }
        .frame(width: 264)
        .tint(.brand)
    }

    private var statusLine: String {
        if model.connected { return "Connected · \(model.bots.count) bot(s)" }
        return model.coreState == .needsConfig ? "Needs configuration" : "Disconnected"
    }
}

/// A full-width, hoverable row used inside the menu-bar popover.
private struct MenuRow: View {
    let title: String
    let systemImage: String
    let action: () -> Void
    @State private var hovering = false

    var body: some View {
        Button(action: action) {
            Label(title, systemImage: systemImage)
                .appFont(.body)
                .frame(maxWidth: .infinity, alignment: .leading)
                .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .padding(.horizontal, 10).padding(.vertical, 6)
        .background(hovering ? Color.brand.opacity(0.15) : .clear,
                    in: RoundedRectangle(cornerRadius: 6, style: .continuous))
        .onHover { hovering = $0 }
        .accessibilityLabel(title)
    }
}
