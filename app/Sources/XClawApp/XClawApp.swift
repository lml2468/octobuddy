import SwiftUI
import XClawCore

@main
struct XClawApp: App {
    @State private var model = AppModel()

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
                .onAppear { if model.coreState == "stopped" { model.start() } }
                .preferredColorScheme(Self.previewScheme)
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

/// The menu-bar status icon. In UI-preview mode it also opens the console on
/// launch (the `Window` scene won't auto-open behind a `MenuBarExtra`).
private struct MenuBarLabel: View {
    @Bindable var model: AppModel
    @Environment(\.openWindow) private var openWindow
    var body: some View {
        Image(systemName: model.connected ? "bolt.horizontal.circle.fill" : "bolt.horizontal.circle")
            .onAppear {
                if ProcessInfo.processInfo.environment["XCLAW_UI_PREVIEW"] != nil {
                    openWindow(id: "console")
                }
            }
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
            transcript
        }
        .safeAreaInset(edge: .bottom, spacing: 0) { composer }
        .toolbar {
            ToolbarItem(placement: .principal) {
                HStack(spacing: 6) {
                    Image(systemName: model.connected ? "circle.fill" : "circle")
                        .font(.system(size: 7))
                        .foregroundStyle(model.connected ? Color.green : Color.secondary)
                    Text(model.selectedBotID ?? "XClaw")
                        .font(.headline)
                    Text("· \(statusSubtitle)")
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }
                .accessibilityElement(children: .combine)
                .accessibilityLabel("\(model.selectedBotID ?? "XClaw"), \(statusSubtitle)")
            }
            ToolbarItemGroup(placement: .primaryAction) {
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
        case "needs-config": return "needs configuration"
        default: return model.connected ? "connected" : model.coreState
        }
    }

    private var transcript: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 10) {
                    if let err = model.lastError, !err.isEmpty {
                        Label(err, systemImage: "exclamationmark.triangle.fill")
                            .font(.caption)
                            .foregroundStyle(.red)
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(10)
                            .background(.red.opacity(0.08), in: RoundedRectangle(cornerRadius: 8, style: .continuous))
                    }

                    let sessions = model.sessions
                    if sessions.isEmpty {
                        ContentUnavailableView(
                            "No Conversations",
                            systemImage: "bubble.left.and.bubble.right",
                            description: Text("Send a message below to talk to the agent.")
                        )
                        .padding(.top, 60)
                    } else {
                        ForEach(sessions) { session in
                            if sessions.count > 1 {
                                Text(session.sessionKey)
                                    .font(.caption.weight(.medium))
                                    .foregroundStyle(.secondary)
                                    .frame(maxWidth: .infinity)
                                    .padding(.top, 6)
                            }
                            ForEach(session.messages) { msg in
                                ChatBubble(message: msg)
                            }
                            if session.awaitingReply {
                                TypingBubble()
                            }
                            if session.outputTokens > 0 {
                                Text("\(session.inputTokens) in · \(session.outputTokens) out")
                                    .font(.caption2)
                                    .foregroundStyle(.tertiary)
                                    .frame(maxWidth: .infinity, alignment: .center)
                            }
                        }
                        Color.clear.frame(height: 1).id("bottom")
                    }
                }
                .padding(16)
                .animation(.smooth, value: model.sessions)
            }
            .scrollContentBackground(.hidden)
            .onChange(of: model.sessions) { _, _ in
                withAnimation(.smooth) { proxy.scrollTo("bottom", anchor: .bottom) }
            }
        }
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

/// One message rendered as a chat bubble: user (trailing, accent), assistant
/// (leading, surface), or a centered tool-call chip.
struct ChatBubble: View {
    let message: AppState.ChatMessage

    var body: some View {
        switch message.role {
        case .user:
            HStack {
                Spacer(minLength: 48)
                Text(message.text)
                    .textSelection(.enabled)
                    .foregroundStyle(.white)
                    .padding(.vertical, 8)
                    .padding(.horizontal, 12)
                    .background(Color.accentColor,
                                in: RoundedRectangle(cornerRadius: 14, style: .continuous))
            }
        case .assistant:
            HStack {
                Text(message.text)
                    .textSelection(.enabled)
                    .foregroundStyle(.primary)
                    .padding(.vertical, 8)
                    .padding(.horizontal, 12)
                    .background(Color(nsColor: .controlBackgroundColor),
                                in: RoundedRectangle(cornerRadius: 14, style: .continuous))
                    .overlay(
                        RoundedRectangle(cornerRadius: 14, style: .continuous)
                            .stroke(.quaternary, lineWidth: 1)
                    )
                Spacer(minLength: 48)
            }
        case .tool:
            HStack {
                Spacer()
                Label(message.text, systemImage: "wrench.and.screwdriver.fill")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .padding(.vertical, 4)
                    .padding(.horizontal, 10)
                    .background(.quaternary, in: Capsule())
                Spacer()
            }
        }
    }
}

/// Animated "agent is typing" indicator shown while awaiting the first output.
struct TypingBubble: View {
    @State private var phase = 0
    private let timer = Timer.publish(every: 0.35, on: .main, in: .common).autoconnect()

    var body: some View {
        HStack {
            HStack(spacing: 4) {
                ForEach(0..<3, id: \.self) { i in
                    Circle()
                        .fill(.secondary)
                        .frame(width: 6, height: 6)
                        .opacity(phase == i ? 1 : 0.3)
                }
            }
            .padding(.vertical, 10)
            .padding(.horizontal, 14)
            .background(Color(nsColor: .controlBackgroundColor),
                        in: RoundedRectangle(cornerRadius: 14, style: .continuous))
            .overlay(
                RoundedRectangle(cornerRadius: 14, style: .continuous)
                    .stroke(.quaternary, lineWidth: 1)
            )
            Spacer(minLength: 48)
        }
        .onReceive(timer) { _ in phase = (phase + 1) % 3 }
        .accessibilityLabel("Agent is replying")
    }
}
