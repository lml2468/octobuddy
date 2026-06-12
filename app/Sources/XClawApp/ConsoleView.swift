import SwiftUI
import AppKit
import XClawCore

/// Prettifies a raw sessionKey: the GUI console session reads as "Console";
/// DM/group keys (`dm:<space>:<uid>`, `group:<channel>`) collapse to "Kind · tail".
func sessionTitle(_ key: String, localUID: String) -> String {
    if key == localUID { return "Console" }
    let parts = key.split(separator: ":")
    if let kind = parts.first, parts.count > 1, let tail = parts.last {
        return "\(kind.capitalized) · \(tail)"
    }
    return key
}

// MARK: - Console (3-column)

/// The main window: Bots │ Conversations │ Transcript.
struct ConsoleView: View {
    @Bindable var model: AppModel

    var body: some View {
        NavigationSplitView {
            BotSidebarColumn(model: model)
                .navigationSplitViewColumnWidth(min: 160, ideal: 184, max: 240)
        } content: {
            ConversationsColumn(model: model)
                .navigationSplitViewColumnWidth(min: 240, ideal: 284, max: 360)
        } detail: {
            TranscriptDetail(model: model)
        }
        .frame(minWidth: 900, minHeight: 600)
    }
}

// MARK: - Column 1: Bots

private struct BotSidebarColumn: View {
    @Bindable var model: AppModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        List(selection: Binding(get: { model.selectedBotID },
                                set: { model.selectedBotID = $0 })) {
            Section {
                if model.roster.isEmpty {
                    Text("No bots configured")
                        .appFont(.callout).foregroundStyle(.secondary)
                }
                ForEach(model.roster) { item in
                    BotRow(item: item)
                        .tag(item.id)
                        .contextMenu {
                            Button {
                                NSApp.activate(ignoringOtherApps: true)
                                openWindow(id: "bot-editor")
                            } label: { Label("Edit Bots…", systemImage: "slider.horizontal.3") }
                            Button {
                                model.selectedBotID = item.id; model.restartCore()
                            } label: { Label("Restart Core", systemImage: "arrow.clockwise") }
                            Divider()
                            Button(role: .destructive) {
                                model.selectedBotID = item.id; model.reset()
                            } label: { Label("Clear Memory", systemImage: "eraser.line.dashed") }
                        }
                }
            } header: {
                Text("Bots").appFont(.caption).foregroundStyle(.secondary)
            }
        }
        .listStyle(.sidebar)
        .scrollContentBackground(.hidden)
        .background(.ultraThinMaterial)
        .animation(AppTheme.spring, value: model.roster.map(\.id))
    }
}

private struct BotRow: View {
    let item: BotRosterItem
    var body: some View {
        HStack(spacing: 9) {
            Circle()
                .fill(item.connected ? Color.green : Color.secondary)
                .frame(width: 8, height: 8)
                .shadow(color: item.connected ? .green.opacity(0.5) : .clear, radius: 3)
            VStack(alignment: .leading, spacing: 1) {
                Text(item.id).appFont(.headline)
                Text(item.connected ? "connected" : (item.lastError ?? "offline"))
                    .appFont(.caption).foregroundStyle(.secondary).lineLimit(1)
            }
            Spacer(minLength: 4)
            if item.sessionCount > 0 {
                Text("\(item.sessionCount)")
                    .appFont(.caption).monospacedDigit()
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, 6).padding(.vertical, 1)
                    .background(.quaternary, in: Capsule())
            }
        }
        .padding(.vertical, 3)
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(item.id), \(item.connected ? "connected" : "offline"), \(item.sessionCount) sessions")
    }
}

// MARK: - Column 2: Conversations

private struct ConversationsColumn: View {
    @Bindable var model: AppModel

    var body: some View {
        Group {
            if let bot = model.currentBot {
                List(selection: Binding(get: { model.selectedSessionKey },
                                        set: { model.selectedSessionKey = $0 })) {
                    Section {
                        ForEach(bot.sortedSessions) { s in
                            ConversationRow(session: s, localUID: model.localUID)
                                .tag(s.sessionKey)
                        }
                    } header: {
                        Text("Conversations").appFont(.caption).foregroundStyle(.secondary)
                    }
                }
                .listStyle(.sidebar)
                .scrollContentBackground(.hidden)
            } else {
                ContentUnavailableView {
                    Label("No Bot Selected", systemImage: "tray")
                } description: {
                    Text("Pick a bot on the left.")
                }
            }
        }
        .onAppear { ensureSelection() }
        .onChange(of: model.selectedBotID) { _, _ in ensureSelection() }
    }

    /// Default the conversation to the bot's Console session when switching bots.
    private func ensureSelection() {
        let keys = model.currentBot?.sortedSessions.map(\.sessionKey) ?? []
        if model.selectedSessionKey == nil || !keys.contains(model.selectedSessionKey!) {
            model.selectedSessionKey = keys.first { $0 == model.localUID } ?? keys.first
        }
    }
}

private struct ConversationRow: View {
    let session: AppState.SessionView
    let localUID: String

    private var subtitle: String {
        if session.awaitingReply { return "replying…" }
        let last = session.messages.last?.text ?? session.lastReply
        return last.isEmpty ? "No messages yet" : last
    }

    var body: some View {
        HStack(spacing: 9) {
            Image(systemName: session.sessionKey == localUID ? "macwindow" : "bubble.left.and.bubble.right")
                .font(.system(size: 13, weight: .medium))
                .symbolRenderingMode(.hierarchical)
                .foregroundStyle(.tint)
                .frame(width: 18)
            VStack(alignment: .leading, spacing: 1) {
                Text(sessionTitle(session.sessionKey, localUID: localUID)).appFont(.headline)
                Text(subtitle).appFont(.caption).foregroundStyle(.secondary).lineLimit(1)
            }
            Spacer(minLength: 0)
            if session.awaitingReply {
                ProgressView().controlSize(.small).scaleEffect(0.7)
            }
        }
        .padding(.vertical, 4)
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(sessionTitle(session.sessionKey, localUID: localUID)), \(subtitle)")
    }
}

// MARK: - Column 3: Transcript + composer

private struct TranscriptDetail: View {
    @Bindable var model: AppModel
    @Environment(\.openWindow) private var openWindow
    @State private var draft: String = ""
    @FocusState private var composerFocused: Bool

    private var currentMessages: [AppState.ChatMessage] { model.currentSession?.messages ?? [] }
    private var isLoadingHistory: Bool {
        guard let id = model.selectedBotID else { return false }
        return model.historyLoadingBots.contains(id) && currentMessages.isEmpty
    }

    var body: some View {
        VStack(spacing: 0) {
            banners
            transcript
        }
        .safeAreaInset(edge: .bottom, spacing: 0) { composer }
        .toolbar { toolbar }
    }

    @ViewBuilder private var banners: some View {
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
                       systemImage: "arrow.clockwise.circle", tint: .brand) {
                Button("Restart now") { model.applyConfigAndRestart() }
            }
        }
    }

    @ToolbarContentBuilder private var toolbar: some ToolbarContent {
        ToolbarItem(placement: .principal) {
            HStack(spacing: 6) {
                Circle().fill(model.connected ? Color.green : Color.secondary).frame(width: 7, height: 7)
                Text(model.selectedBotID ?? "XClaw").appFont(.headline)
                Text("· \(statusSubtitle)").appFont(.caption).foregroundStyle(.secondary)
            }
            .accessibilityElement(children: .combine)
            .accessibilityLabel("\(model.selectedBotID ?? "XClaw"), \(statusSubtitle)")
        }
        ToolbarItemGroup(placement: .primaryAction) {
            Button { model.reset() } label: { Image(systemName: "eraser.line.dashed") }
                .help("Clear this bot's conversation memory")
                .accessibilityLabel("Clear conversation memory")
            Button { model.restartCore() } label: { Image(systemName: "arrow.clockwise") }
                .help("Restart the xclawd core process")
                .accessibilityLabel("Restart core")
        }
    }

    private var statusSubtitle: String {
        switch model.coreState {
        case .needsConfig: return "needs configuration"
        default: return model.connected ? "connected" : model.coreState.display
        }
    }

    private var transcript: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 10) {
                    if let err = model.lastError, !err.isEmpty {
                        Label(err, systemImage: "exclamationmark.triangle.fill")
                            .appFont(.caption).foregroundStyle(.red)
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(10)
                            .background(.red.opacity(0.08), in: RoundedRectangle(cornerRadius: 8, style: .continuous))
                    }

                    if isLoadingHistory {
                        ForEach(0..<5, id: \.self) { i in MessageSkeleton(fromUser: i % 2 == 1) }
                    } else if currentMessages.isEmpty {
                        emptyState
                    } else {
                        ForEach(currentMessages) { msg in
                            ChatBubble(message: msg)
                                .transition(.asymmetric(
                                    insertion: .scale(scale: 0.96, anchor: .bottom).combined(with: .opacity),
                                    removal: .opacity))
                        }
                        if model.currentSession?.awaitingReply == true {
                            TypingBubble()
                                .transition(.opacity.combined(with: .scale(scale: 0.92, anchor: .leading)))
                        }
                        if let s = model.currentSession, s.outputTokens > 0 {
                            Text("\(s.inputTokens) in · \(s.outputTokens) out")
                                .appFont(.caption).monospacedDigit()
                                .foregroundStyle(.tertiary)
                                .frame(maxWidth: .infinity, alignment: .center)
                                .padding(.top, 2)
                        }
                        Color.clear.frame(height: 1).id("bottom")
                    }
                }
                .padding(20)
                .animation(AppTheme.spring, value: currentMessages.count)
            }
            .scrollContentBackground(.hidden)
            .onChange(of: currentMessages.count) { _, _ in proxy.scrollTo("bottom", anchor: .bottom) }
            .onChange(of: model.currentSession) { _, _ in proxy.scrollTo("bottom", anchor: .bottom) }
            .onChange(of: model.selectedSessionKey) { _, _ in proxy.scrollTo("bottom", anchor: .bottom) }
        }
    }

    private var emptyState: some View {
        ContentUnavailableView {
            Label {
                Text("Start a conversation").appFont(.title)
            } icon: {
                OctopusShape().fill(style: FillStyle(eoFill: true))
                    .foregroundStyle(Color.brand)
                    .frame(width: 52, height: 52)
            }
        } description: {
            Text("Send a message below to talk to the agent.").appFont(.body).foregroundStyle(.secondary)
        }
        .padding(.top, 60)
    }

    private var canSend: Bool {
        model.connected && !draft.trimmingCharacters(in: .whitespaces).isEmpty
    }

    private var composer: some View {
        HStack(alignment: .bottom, spacing: 8) {
            TextField("Message the agent…", text: $draft, axis: .vertical)
                .textFieldStyle(.plain)
                .appFont(.body)
                .lineLimit(1...6)
                .focused($composerFocused)
                .onSubmit(sendDraft)
                .padding(.horizontal, 12)
                .padding(.vertical, 9)
                .background(Color(nsColor: .textBackgroundColor),
                            in: RoundedRectangle(cornerRadius: 10, style: .continuous))
                .focusRing(composerFocused, cornerRadius: 10)
            Button(action: sendDraft) {
                Image(systemName: "arrow.up.circle.fill")
                    .font(.system(size: 26))
                    .symbolRenderingMode(.hierarchical)
            }
            .buttonStyle(.plain)
            .foregroundStyle(canSend ? Color.brand : Color.secondary)
            .disabled(!canSend)
            .keyboardShortcut(.return, modifiers: [])
            .help("Send (Return)")
            .accessibilityLabel("Send message")
            .hoverScale(1.08)
        }
        .padding(12)
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

/// A thin, glassy info bar shown above the content (needs-config, restart…).
private struct InfoBanner<Trailing: View>: View {
    let text: String
    let systemImage: String
    let tint: Color
    @ViewBuilder var trailing: Trailing

    var body: some View {
        HStack(spacing: 8) {
            Image(systemName: systemImage).foregroundStyle(tint)
            Text(text).appFont(.body)
            Spacer()
            trailing.controlSize(.small)
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 10)
        .liquidGlass(in: Rectangle(), fallback: .regularMaterial)
        .overlay(alignment: .bottom) { Divider().opacity(0.5) }
    }
}

/// One message: tool calls render as a centered chip; user/assistant as a bubble.
struct ChatBubble: View {
    let message: AppState.ChatMessage

    var body: some View {
        if message.role == .tool {
            HStack {
                Spacer()
                Label(message.text, systemImage: "wrench.and.screwdriver.fill")
                    .appFont(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                    .capsuleTag()
                Spacer()
            }
        } else {
            BubbleRow(message: message)
        }
    }
}

/// A user/assistant bubble that reveals a timestamp + copy button on hover.
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
            if isUser { Spacer(minLength: 56) }
            content
                .padding(.vertical, 9)
                .padding(.horizontal, 13)
                .background(isUser ? AnyShapeStyle(Color.brand)
                                   : AnyShapeStyle(.thickMaterial),
                            in: RoundedRectangle(cornerRadius: 15, style: .continuous))
                .overlay {
                    if !isUser {
                        RoundedRectangle(cornerRadius: 15, style: .continuous)
                            .strokeBorder(.quaternary, lineWidth: 1)
                    }
                }
                .floatingShadow()
                .contextMenu {
                    Button { copy() } label: { Label("Copy", systemImage: "doc.on.doc") }
                }
            if !isUser { Spacer(minLength: 56) }
        }
    }

    /// User text stays plain (white on the brand bubble); assistant text renders
    /// Markdown (inline styling + fenced code panels), width-capped for readability.
    @ViewBuilder private var content: some View {
        if isUser {
            Text(message.text).appFont(.body).textSelection(.enabled).foregroundStyle(.white)
        } else {
            MarkdownMessage(text: message.text)
                .appFont(.body)
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
            Text(message.timestamp, format: .dateTime.hour().minute()).monospacedDigit()
            if !isUser { copyButton }
        }
        .appFont(.caption)
        .foregroundStyle(.secondary)
    }

    private var copyButton: some View {
        Button { copy() } label: { Image(systemName: "doc.on.doc") }
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
                        .scaleEffect(phase == i ? 1.15 : 1)
                        .animation(AppTheme.spring, value: phase)
                }
            }
            .padding(.vertical, 11)
            .padding(.horizontal, 15)
            .background(.thickMaterial, in: RoundedRectangle(cornerRadius: 15, style: .continuous))
            .overlay(RoundedRectangle(cornerRadius: 15, style: .continuous).strokeBorder(.quaternary, lineWidth: 1))
            .floatingShadow()
            Spacer(minLength: 56)
        }
        .onReceive(timer) { _ in phase = (phase + 1) % 3 }
        .accessibilityLabel("Agent is replying")
    }
}

/// A shimmering placeholder bubble shown while a transcript's history loads.
private struct MessageSkeleton: View {
    let fromUser: Bool
    var body: some View {
        HStack {
            if fromUser { Spacer(minLength: 80) }
            RoundedRectangle(cornerRadius: 15, style: .continuous)
                .fill(.quaternary)
                .frame(width: fromUser ? 180 : 280, height: 38)
                .shimmering()
            if !fromUser { Spacer(minLength: 80) }
        }
    }
}
