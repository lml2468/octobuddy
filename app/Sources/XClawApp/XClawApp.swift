import SwiftUI
import XClawCore

@main
struct XClawApp: App {
    @State private var model = AppModel()

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
            Image(systemName: model.connected ? "bolt.horizontal.circle.fill" : "bolt.horizontal.circle")
        }
        .menuBarExtraStyle(.window)

        Window("XClaw", id: "console") {
            ConsoleView(model: model)
                .onAppear { if model.coreState == "stopped" { model.start() } }
        }
        .defaultSize(width: 880, height: 600)
        .windowToolbarStyle(.unified)

        // Config editor (Cmd-,). Loads the on-disk config when opened.
        Settings {
            ConfigEditorView(config: model.config,
                             onSaveAndRestart: { model.applyConfigAndRestart() })
                .onAppear { model.config.load() }
        }
    }
}

// MARK: - Menu bar popover

/// Menu-bar dropdown — a small status card with quick actions. Uses
/// `@Environment(\.openWindow)` to surface the console window.
private struct MenuBarContent: View {
    @Bindable var model: AppModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 10) {
                Image(systemName: "bolt.horizontal.circle.fill")
                    .font(.title2)
                    .foregroundStyle(model.connected ? Color.green : Color.secondary)
                VStack(alignment: .leading, spacing: 1) {
                    Text("XClaw").font(.headline)
                    Text(statusLine).font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
            }
            .padding(.horizontal, 14)
            .padding(.vertical, 12)

            Divider()

            VStack(spacing: 2) {
                MenuRow(title: "Open Console", systemImage: "macwindow") {
                    NSApp.activate(ignoringOtherApps: true)
                    openWindow(id: "console")
                }
                SettingsLink {
                    Label("Edit Bots…", systemImage: "slider.horizontal.3")
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
                .buttonStyle(.plain)
                .padding(.horizontal, 10).padding(.vertical, 6)
                MenuRow(title: "Restart Core", systemImage: "arrow.clockwise") {
                    model.stop(); model.start()
                }
                Divider().padding(.vertical, 4)
                MenuRow(title: "Quit XClaw", systemImage: "power") {
                    model.stop(); NSApplication.shared.terminate(nil)
                }
            }
            .padding(8)
        }
        .frame(width: 260)
    }

    private var statusLine: String {
        if model.connected { return "Connected · \(model.bots.count) bot(s)" }
        return model.coreState == "needs-config" ? "Needs configuration" : "Disconnected"
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
                .frame(maxWidth: .infinity, alignment: .leading)
                .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .padding(.horizontal, 10).padding(.vertical, 6)
        .background(hovering ? Color.accentColor.opacity(0.15) : .clear,
                    in: RoundedRectangle(cornerRadius: 6, style: .continuous))
        .onHover { hovering = $0 }
    }
}

// MARK: - Console

struct ConsoleView: View {
    @Bindable var model: AppModel
    @State private var draft: String = ""
    @FocusState private var composerFocused: Bool

    var body: some View {
        NavigationSplitView {
            botSidebar
                .navigationSplitViewColumnWidth(min: 200, ideal: 240, max: 320)
        } detail: {
            detail
        }
        .frame(minWidth: 720, minHeight: 460)
    }

    // MARK: sidebar

    private var botSidebar: some View {
        List(selection: Binding(
            get: { model.selectedBotID },
            set: { model.selectedBotID = $0 }
        )) {
            Section("Bots") {
                if model.bots.isEmpty {
                    Text("No bots configured")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                }
                ForEach(model.bots) { bot in
                    Label {
                        VStack(alignment: .leading, spacing: 1) {
                            Text(bot.id)
                            Text(bot.connected ? "connected" : (bot.lastError ?? "offline"))
                                .font(.caption2)
                                .foregroundStyle(.secondary)
                                .lineLimit(1)
                        }
                    } icon: {
                        Image(systemName: bot.connected ? "circle.fill" : "circle")
                            .font(.system(size: 9))
                            .foregroundStyle(bot.connected ? Color.green : Color.secondary)
                    }
                    .badge(bot.sessions.count)
                    .tag(bot.id)
                    .accessibilityElement(children: .combine)
                    .accessibilityLabel("\(bot.id), \(bot.connected ? "connected" : "offline"), \(bot.sessions.count) sessions")
                }
            }
        }
        .listStyle(.sidebar)
        .animation(.smooth, value: model.bots)
    }

    // MARK: detail

    private var detail: some View {
        VStack(spacing: 0) {
            if model.coreState == "needs-config" {
                InfoBanner(text: "No bots configured. Add one to get started.",
                           systemImage: "gearshape.badge.exclamationmark", tint: .orange) {
                    SettingsLink { Text("Edit Bots…") }
                }
            }
            if model.config.needsRestart {
                InfoBanner(text: "Configuration changed — restart the core to apply.",
                           systemImage: "arrow.clockwise.circle", tint: .accentColor) {
                    Button("Restart now") { model.applyConfigAndRestart() }
                }
            }
            sessionList
        }
        .safeAreaInset(edge: .bottom, spacing: 0) { composer }
        .navigationTitle(model.selectedBotID ?? "XClaw")
        .navigationSubtitle(statusSubtitle)
        .toolbar {
            ToolbarItemGroup(placement: .primaryAction) {
                Image(systemName: model.connected ? "bolt.horizontal.circle.fill" : "bolt.horizontal.circle")
                    .foregroundStyle(model.connected ? Color.green : Color.secondary)
                    .help(model.connected ? "Bus connected" : "Bus disconnected")
                Button { model.reset() } label: {
                    Image(systemName: "eraser.line.dashed")
                }
                .help("Clear this bot's conversation memory")
                Button { model.stop(); model.start() } label: {
                    Image(systemName: "arrow.clockwise")
                }
                .help("Restart the xclawd core process")
            }
        }
    }

    private var statusSubtitle: String {
        switch model.coreState {
        case "needs-config": return "Needs configuration"
        default: return model.connected ? "Connected" : model.coreState
        }
    }

    private var sessionList: some View {
        ScrollView {
            LazyVStack(alignment: .leading, spacing: 12) {
                if let err = model.lastError, !err.isEmpty {
                    Label(err, systemImage: "exclamationmark.triangle.fill")
                        .font(.caption)
                        .foregroundStyle(.red)
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(10)
                        .background(.red.opacity(0.08), in: RoundedRectangle(cornerRadius: 8, style: .continuous))
                }
                if model.sessions.isEmpty {
                    ContentUnavailableView(
                        "No Sessions",
                        systemImage: "bubble.left.and.bubble.right",
                        description: Text("Send a message below to start a conversation.")
                    )
                    .padding(.top, 60)
                } else {
                    ForEach(model.sessions, id: \.sessionKey) { s in
                        SessionRow(session: s)
                    }
                }
            }
            .padding(16)
            .animation(.smooth, value: model.sessions)
        }
        .scrollContentBackground(.hidden)
    }

    private var canSend: Bool {
        model.connected && !draft.trimmingCharacters(in: .whitespaces).isEmpty
    }

    private var composer: some View {
        HStack(alignment: .bottom, spacing: 8) {
            TextField("Message the agent…", text: $draft, axis: .vertical)
                .textFieldStyle(.plain)
                .lineLimit(1...5)
                .focused($composerFocused)
                .onSubmit(sendDraft)
                .padding(.horizontal, 11)
                .padding(.vertical, 8)
                .background(Color(nsColor: .textBackgroundColor),
                            in: RoundedRectangle(cornerRadius: 9, style: .continuous))
                .overlay(
                    RoundedRectangle(cornerRadius: 9, style: .continuous)
                        .stroke(.quaternary, lineWidth: 1)
                )
            Button(action: sendDraft) {
                Image(systemName: "arrow.up.circle.fill")
                    .font(.system(size: 24))
                    .symbolRenderingMode(.hierarchical)
            }
            .buttonStyle(.plain)
            .foregroundStyle(canSend ? Color.accentColor : Color.secondary)
            .disabled(!canSend)
            .keyboardShortcut(.return, modifiers: [])
            .help("Send (Return)")
        }
        .padding(10)
        .background(.bar)
    }

    private func sendDraft() {
        let text = draft
        draft = ""
        model.send(text)
        composerFocused = true
    }
}

// MARK: - Components

/// A thin, material info bar shown above the content (needs-config, restart…).
private struct InfoBanner<Trailing: View>: View {
    let text: String
    let systemImage: String
    let tint: Color
    @ViewBuilder var trailing: Trailing

    var body: some View {
        HStack(spacing: 8) {
            Image(systemName: systemImage).foregroundStyle(tint)
            Text(text).font(.callout)
            Spacer()
            trailing
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 9)
        .background(.regularMaterial)
        .overlay(alignment: .bottom) { Divider() }
    }
}

/// One session rendered as a clean card: header (key + activity), the agent's
/// text, an optional tool chip, and a token-usage footer.
struct SessionRow: View {
    let session: AppState.SessionView

    private var bodyText: String {
        session.streamingText.isEmpty ? session.lastReply : session.streamingText
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 6) {
                Image(systemName: "person.crop.circle")
                    .foregroundStyle(.tint)
                Text(session.sessionKey)
                    .font(.subheadline.weight(.semibold))
                Spacer()
                if !session.lastActivity.isEmpty {
                    Text(session.lastActivity)
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                        .padding(.horizontal, 7)
                        .padding(.vertical, 2)
                        .background(.quaternary, in: Capsule())
                }
            }

            if !bodyText.isEmpty {
                Text(bodyText)
                    .font(.callout)
                    .foregroundStyle(.primary)
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }

            if !session.lastTool.isEmpty {
                Label(session.lastTool, systemImage: "wrench.and.screwdriver.fill")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, 8)
                    .padding(.vertical, 3)
                    .background(.quaternary, in: Capsule())
            }

            if session.outputTokens > 0 {
                Text("\(session.inputTokens) in · \(session.outputTokens) out")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
            }
        }
        .padding(13)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color(nsColor: .controlBackgroundColor),
                    in: RoundedRectangle(cornerRadius: 12, style: .continuous))
        .overlay(
            RoundedRectangle(cornerRadius: 12, style: .continuous)
                .stroke(.quaternary, lineWidth: 1)
        )
        .shadow(color: .black.opacity(0.05), radius: 3, y: 1)
        .accessibilityElement(children: .combine)
    }
}
