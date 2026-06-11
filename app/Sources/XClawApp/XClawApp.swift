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
        // Menu bar presence: status + quick actions.
        MenuBarExtra("XClaw", systemImage: model.connected ? "bolt.horizontal.circle.fill" : "bolt.horizontal.circle") {
            MenuBarContent(model: model)
        }

        Window("XClaw Console", id: "console") {
            ConsoleView(model: model)
                .onAppear { if model.coreState == "stopped" { model.start() } }
        }
        .defaultSize(width: 820, height: 560)

        // Config editor (Cmd-,). Loads the on-disk config when opened.
        Settings {
            ConfigEditorView(config: model.config,
                             onSaveAndRestart: { model.applyConfigAndRestart() })
                .onAppear { model.config.load() }
        }
    }
}

/// Menu-bar dropdown contents. Split out so it can use `@Environment(\.openWindow)`
/// (the modern, title-string-free way to surface the console window).
private struct MenuBarContent: View {
    @Bindable var model: AppModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Text("Core: \(model.coreState)")
        Text(model.connected ? "Bus: connected · \(model.bots.count) bot(s)" : "Bus: disconnected")
        Divider()
        Button("Open Console") {
            NSApp.activate(ignoringOtherApps: true)
            openWindow(id: "console")
        }
        SettingsLink { Text("Edit Bots…") }
        Button("Restart Core") { model.stop(); model.start() }
        Divider()
        Button("Quit") { model.stop(); NSApplication.shared.terminate(nil) }
            .keyboardShortcut("q")
    }
}

struct ConsoleView: View {
    @Bindable var model: AppModel
    @State private var draft: String = ""
    @FocusState private var composerFocused: Bool

    var body: some View {
        NavigationSplitView {
            botSidebar
                .navigationSplitViewColumnWidth(min: 180, ideal: 220, max: 300)
        } detail: {
            VStack(spacing: 0) {
                if model.coreState == "needs-config" {
                    needsConfigBanner
                }
                if model.config.needsRestart {
                    restartBanner
                }
                header
                Divider()
                sessionList
                Divider()
                composer
            }
        }
        .frame(minWidth: 680, minHeight: 420)
    }

    private var needsConfigBanner: some View {
        HStack {
            Image(systemName: "gearshape.badge.exclamationmark")
            Text("No bots configured. Add one to get started.")
            Spacer()
            SettingsLink { Text("Edit Bots…") }
        }
        .padding(10)
        .background(Color.orange.opacity(0.15))
    }

    private var restartBanner: some View {
        HStack {
            Image(systemName: "arrow.clockwise.circle")
            Text("Config changed — restart the core to apply.")
            Spacer()
            Button("Restart now") { model.applyConfigAndRestart() }
        }
        .padding(10)
        .background(Color.blue.opacity(0.12))
    }

    private var botSidebar: some View {
        List(selection: Binding(
            get: { model.selectedBotID },
            set: { model.selectedBotID = $0 }
        )) {
            Section("Bots") {
                if model.bots.isEmpty {
                    Text("No bots").foregroundStyle(.secondary)
                }
                ForEach(model.bots) { bot in
                    HStack(spacing: 8) {
                        Circle()
                            .fill(bot.connected ? Color.green : Color.orange)
                            .frame(width: 8, height: 8)
                            .accessibilityHidden(true) // status is in the text below
                        VStack(alignment: .leading, spacing: 1) {
                            Text(bot.id).font(.body)
                            Text(bot.connected ? "connected" : "offline")
                                .font(.caption2).foregroundStyle(.secondary)
                        }
                        Spacer()
                        if !bot.sessions.isEmpty {
                            Text("\(bot.sessions.count)")
                                .font(.caption2).foregroundStyle(.secondary)
                                .accessibilityLabel("\(bot.sessions.count) sessions")
                        }
                    }
                    .tag(bot.id)
                    .accessibilityElement(children: .combine)
                    .accessibilityLabel("\(bot.id), \(bot.connected ? "connected" : "offline"), \(bot.sessions.count) sessions")
                }
            }
        }
    }

    private var header: some View {
        HStack(spacing: 10) {
            Circle()
                .fill(model.connected ? Color.green : Color.orange)
                .frame(width: 10, height: 10)
                .accessibilityLabel(model.connected ? "Bus connected" : "Bus disconnected")
            VStack(alignment: .leading, spacing: 2) {
                Text(model.selectedBotID ?? "xclaw core").font(.headline)
                Text(model.coreState).font(.caption).foregroundStyle(.secondary)
            }
            Spacer()
            Button("Reset") { model.reset() }
                .help("Clear this bot's conversation memory")
            Button(model.connected ? "Restart" : "Start") {
                model.stop(); model.start()
            }
            .help("Restart the xclawd core process")
        }
        .padding(12)
    }

    private var sessionList: some View {
        ScrollView {
            LazyVStack(alignment: .leading, spacing: 12) {
                if let err = model.lastError {
                    Label(err, systemImage: "exclamationmark.triangle")
                        .font(.caption)
                        .foregroundStyle(.red)
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
                if model.sessions.isEmpty {
                    ContentUnavailableView(
                        "No Sessions",
                        systemImage: "bubble.left.and.bubble.right",
                        description: Text("Send a message below to start a conversation.")
                    )
                    .frame(maxWidth: .infinity)
                    .padding(.top, 40)
                }
                ForEach(model.sessions, id: \.sessionKey) { s in
                    SessionRow(session: s)
                }
            }
            .padding(12)
        }
    }

    private var composer: some View {
        HStack(spacing: 8) {
            TextField("Message the agent…", text: $draft, axis: .vertical)
                .textFieldStyle(.roundedBorder)
                .lineLimit(1...4)
                .focused($composerFocused)
                .onSubmit(sendDraft)
            Button("Send", action: sendDraft)
                .keyboardShortcut(.return, modifiers: [])
                .disabled(!model.connected || draft.trimmingCharacters(in: .whitespaces).isEmpty)
        }
        .padding(12)
    }

    private func sendDraft() {
        let text = draft
        draft = ""
        model.send(text)
        composerFocused = true // keep focus for the next message
    }
}

struct SessionRow: View {
    let session: AppState.SessionView

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text(session.sessionKey).font(.subheadline.bold())
                Spacer()
                Text(session.lastActivity).font(.caption).foregroundStyle(.secondary)
                if session.outputTokens > 0 {
                    Text("· \(session.inputTokens)→\(session.outputTokens) tok")
                        .font(.caption2).foregroundStyle(.secondary)
                }
            }
            if !session.lastTool.isEmpty {
                Label(session.lastTool, systemImage: "wrench.and.screwdriver")
                    .font(.caption).foregroundStyle(.secondary)
            }
            // Live streaming text takes precedence; fall back to last reply.
            let body = session.streamingText.isEmpty ? session.lastReply : session.streamingText
            if !body.isEmpty {
                Text(body)
                    .font(.callout)
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
        .padding(10)
        .background(Color.secondary.opacity(0.08), in: RoundedRectangle(cornerRadius: 8))
        .accessibilityElement(children: .combine)
    }
}
