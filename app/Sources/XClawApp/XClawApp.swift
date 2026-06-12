import SwiftUI
import AppKit
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
                .onAppear { if model.coreState == .stopped { model.start() } }
                .tint(.brand)
                .preferredColorScheme(Self.previewScheme)
        }
        .defaultSize(width: 880, height: 600)
        .windowToolbarStyle(.unified)
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
                .preferredColorScheme(Self.previewScheme)
        }
        .defaultSize(width: 820, height: 620)
        .windowToolbarStyle(.unified)
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
            } else {
                Image(systemName: model.connected ? "bolt.horizontal.circle.fill" : "bolt.horizontal.circle")
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
                MenuRow(title: "Edit Bots…", systemImage: "slider.horizontal.3") {
                    NSApp.activate(ignoringOtherApps: true)
                    openWindow(id: "bot-editor")
                }
                MenuRow(title: "Restart Core", systemImage: "arrow.clockwise") {
                    model.restartCore()
                }
                Divider().padding(.vertical, 4)
                MenuRow(title: "Quit XClaw", systemImage: "power") {
                    model.stop(); NSApplication.shared.terminate(nil)
                }
            }
            .padding(8)
        }
        .frame(width: 260)
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

// MARK: - Console

struct ConsoleView: View {
    @Bindable var model: AppModel
    @Environment(\.openWindow) private var openWindow
    @State private var draft: String = ""
    @State private var selectedSessionKey: String?
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
                    .contextMenu {
                        Button {
                            NSApp.activate(ignoringOtherApps: true)
                            openWindow(id: "bot-editor")
                        } label: { Label("Edit Bots…", systemImage: "slider.horizontal.3") }
                        Button {
                            model.selectedBotID = bot.id
                            model.restartCore()
                        } label: { Label("Restart Core", systemImage: "arrow.clockwise") }
                        Divider()
                        Button(role: .destructive) {
                            model.selectedBotID = bot.id
                            model.reset()
                        } label: { Label("Clear Memory", systemImage: "eraser.line.dashed") }
                    }
                }
            }
        }
        .listStyle(.sidebar)
        .animation(.smooth, value: model.bots)
    }

    // MARK: detail

    private var detail: some View {
        VStack(spacing: 0) {
            if model.coreState == .needsConfig {
                InfoBanner(text: "No bots configured. Add one to get started.",
                           systemImage: "gearshape.badge.exclamationmark", tint: .orange) {
                    Button("Edit Bots…") {
                        NSApp.activate(ignoringOtherApps: true)
                        openWindow(id: "bot-editor")
                    }
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
                .accessibilityLabel("Clear conversation memory")
                Button { model.restartCore() } label: {
                    Image(systemName: "arrow.clockwise")
                }
                .help("Restart the xclawd core process")
                .accessibilityLabel("Restart core")
            }
        }
    }

    private var statusSubtitle: String {
        switch model.coreState {
        case .needsConfig: return "needs configuration"
        default: return model.connected ? "connected" : model.coreState.display
        }
    }

    /// Prettifies a raw sessionKey for the session picker: the GUI console
    /// session reads as "Console"; DM/group keys (`dm:<space>:<uid>`,
    /// `group:<channel>`) collapse to their trailing identifier.
    private func sessionTitle(_ key: String) -> String {
        if key == model.localUID { return "Console" }
        let parts = key.split(separator: ":")
        if let kind = parts.first, parts.count > 1, let tail = parts.last {
            return "\(kind.capitalized) · \(tail)"
        }
        return key
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
                        let current = sessions.first { $0.sessionKey == selectedSessionKey } ?? sessions[0]
                        if sessions.count > 1 {
                            Picker("Session", selection: Binding(
                                get: { current.sessionKey },
                                set: { selectedSessionKey = $0 }
                            )) {
                                ForEach(sessions) { Text(sessionTitle($0.sessionKey)).tag($0.sessionKey) }
                            }
                            .pickerStyle(.segmented)
                            .labelsHidden()
                            .padding(.bottom, 6)
                        }
                        ForEach(current.messages) { msg in
                            ChatBubble(message: msg)
                                .transition(.asymmetric(
                                    insertion: .opacity.combined(with: .move(edge: .bottom)),
                                    removal: .opacity))
                        }
                        if current.awaitingReply {
                            TypingBubble()
                                .transition(.opacity.combined(with: .scale(scale: 0.92, anchor: .leading)))
                        }
                        if current.outputTokens > 0 {
                            Text("\(current.inputTokens) in · \(current.outputTokens) out")
                                .font(.caption2)
                                .foregroundStyle(.tertiary)
                                .frame(maxWidth: .infinity, alignment: .center)
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
            .foregroundStyle(canSend ? Color.brand : Color.secondary)
            .disabled(!canSend)
            .keyboardShortcut(.return, modifiers: [])
            .help("Send (Return)")
            .accessibilityLabel("Send message")
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
        .liquidGlass(in: Rectangle(), fallback: .regularMaterial)
        .overlay(alignment: .bottom) { Divider() }
    }
}

/// One message: tool calls render as a centered chip; user/assistant render as
/// a `BubbleRow` (with a hover timestamp/copy affordance).
struct ChatBubble: View {
    let message: AppState.ChatMessage

    var body: some View {
        if message.role == .tool {
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
        } else {
            BubbleRow(message: message)
        }
    }
}

/// A user/assistant message bubble that reveals a timestamp + copy button on
/// hover.
private struct BubbleRow: View {
    let message: AppState.ChatMessage
    @State private var hovering = false

    private var isUser: Bool { message.role == .user }

    var body: some View {
        VStack(alignment: isUser ? .trailing : .leading, spacing: 2) {
            bubble
            meta
                .opacity(hovering ? 1 : 0)
                .frame(height: 14)
                .allowsHitTesting(hovering)
        }
        .frame(maxWidth: .infinity, alignment: isUser ? .trailing : .leading)
        .onHover { hovering = $0 }
    }

    private var bubble: some View {
        HStack(spacing: 0) {
            if isUser { Spacer(minLength: 48) }
            content
                .padding(.vertical, 8)
                .padding(.horizontal, 12)
                .background(isUser ? AnyShapeStyle(Color.brand)
                                   : AnyShapeStyle(Color(nsColor: .controlBackgroundColor)),
                            in: RoundedRectangle(cornerRadius: 14, style: .continuous))
                .overlay {
                    if !isUser {
                        RoundedRectangle(cornerRadius: 14, style: .continuous)
                            .stroke(.quaternary, lineWidth: 1)
                    }
                }
                .floatingShadow()
                .contextMenu {
                    Button { copy() } label: { Label("Copy", systemImage: "doc.on.doc") }
                }
            if !isUser { Spacer(minLength: 48) }
        }
    }

    /// User text stays plain (white on the brand bubble); assistant text renders
    /// Markdown (inline styling + fenced code panels) and is width-capped so long
    /// agent output stays readable on a wide window.
    @ViewBuilder private var content: some View {
        if isUser {
            Text(message.text)
                .textSelection(.enabled)
                .foregroundStyle(.white)
        } else {
            MarkdownMessage(text: message.text)
                .frame(maxWidth: 640, alignment: .leading)
        }
    }

    private func copy() {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(message.text, forType: .string)
    }

    private var meta: some View {
        HStack(spacing: 6) {
            if isUser { copyButton }
            Text(message.timestamp, format: .dateTime.hour().minute())
            if !isUser { copyButton }
        }
        .font(.caption2)
        .foregroundStyle(.secondary)
    }

    private var copyButton: some View {
        Button {
            copy()
        } label: {
            Image(systemName: "doc.on.doc")
        }
        .buttonStyle(.borderless)
        .help("Copy message")
        .accessibilityLabel("Copy message")
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
