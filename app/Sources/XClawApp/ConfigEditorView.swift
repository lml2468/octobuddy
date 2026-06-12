import SwiftUI
import XClawCore

/// Bot configuration editor (its own window, opened via ⌘, or the menu-bar
/// "Edit Bots…"). Lists configured bots, lets you add/remove and edit identity /
/// connection / agent / persona / environment, and saves to ~/.xclaw/config.json
/// (tokens → Keychain, persona → SOUL.md / AGENTS.md). See ConfigEditorModel /
/// ConfigStore / Keychain.
struct ConfigEditorView: View {
    @Bindable var config: ConfigEditorModel
    /// Invoked when the user chooses "Save & Restart" after a successful save.
    var onSaveAndRestart: () -> Void
    @State private var selection: UUID?

    var body: some View {
        NavigationSplitView {
            List(selection: $selection) {
                Section("Bots") {
                    ForEach($config.bots, id: \.rowID) { $bot in
                        Label {
                            VStack(alignment: .leading, spacing: 1) {
                                Text(bot.id.isEmpty ? "(unnamed)" : bot.id)
                                if !bot.apiURL.isEmpty {
                                    Text(bot.apiURL)
                                        .font(.caption2)
                                        .foregroundStyle(.secondary)
                                        .lineLimit(1)
                                        .truncationMode(.middle)
                                }
                            }
                        } icon: {
                            Image(systemName: "person.crop.square")
                                .foregroundStyle(.tint)
                        }
                        .tag(bot.rowID)
                        .contextMenu {
                            Button(role: .destructive) {
                                let rid = bot.rowID
                                config.remove(rowID: rid)
                                if selection == rid { selection = config.bots.first?.rowID }
                            } label: { Label("Remove Bot", systemImage: "trash") }
                        }
                    }
                    .onDelete { idx in
                        let ids = idx.map { config.bots[$0].rowID }
                        ids.forEach { config.remove(rowID: $0) }
                    }
                }
            }
            .frame(minWidth: 200)
            .listStyle(.sidebar)
            .scrollContentBackground(.hidden)
            .background(.ultraThinMaterial)
            .toolbar {
                ToolbarItem {
                    Button {
                        config.add()
                        selection = config.bots.last?.rowID
                    } label: { Image(systemName: "plus") }
                    .help("Add a bot")
                    .accessibilityLabel("Add a bot")
                }
            }
        } detail: {
            // Identify the selected bot by its stable rowID, not the editable
            // slug — so editing the Bot ID field doesn't desync selection and
            // collapse the form mid-keystroke.
            if let sel = selection, let i = config.bots.firstIndex(where: { $0.rowID == sel }) {
                BotForm(bot: $config.bots[i], onRemove: {
                    config.remove(rowID: sel)
                    selection = config.bots.first?.rowID
                })
                .id(sel) // rebuild cleanly when switching bots
            } else {
                ContentUnavailableView {
                    Label {
                        Text("No Bot Selected").appFont(.title)
                    } icon: {
                        OctopusShape().fill(style: FillStyle(eoFill: true))
                            .foregroundStyle(Color.brand)
                            .frame(width: 48, height: 48)
                    }
                } description: {
                    Text("Select a bot on the left, or add one with +.")
                        .appFont(.body).foregroundStyle(.secondary)
                }
            }
        }
        .frame(minWidth: 820, minHeight: 560)
        .safeAreaInset(edge: .bottom) { footer }
    }

    private var footer: some View {
        HStack {
            if let err = config.error {
                Label(err, systemImage: "exclamationmark.triangle.fill")
                    .appFont(.caption).foregroundStyle(.red).lineLimit(2)
            } else if config.needsRestart {
                Label("Saved. Restart the core to apply.", systemImage: "checkmark.circle.fill")
                    .appFont(.caption).foregroundStyle(.green)
            }
            Spacer()
            Button("Save") { config.save() }
                .keyboardShortcut("s", modifiers: .command)
            Button("Save & Restart") {
                if config.save() { onSaveAndRestart() }
            }
            .buttonStyle(.borderedProminent)
        }
        .padding(12)
        .background(.bar)
    }
}

// MARK: - Bot form

private struct BotForm: View {
    @Binding var bot: BotConfig
    var onRemove: () -> Void

    var body: some View {
        Form {
            Section {
                textRow("Bot ID", prompt: "my-bot", $bot.id,
                        help: "Letters, digits, dot, underscore, hyphen. Used as the subtree name under ~/.xclaw.")
                if !ConfigStore.validSlug(bot.id) {
                    warning("Invalid id — letters, digits, . _ - only")
                }
            } header: {
                Label("Identity", systemImage: "person.text.rectangle")
            }

            Section {
                textRow("API URL", prompt: "https://your-octo-server", $bot.apiURL, validateURL: true)
                secureRow("Bot Token", prompt: "bf_…", $bot.octoToken,
                          help: "Stored in your macOS Keychain, not in config.json.")
            } header: {
                Label("Connection", systemImage: "antenna.radiowaves.left.and.right")
            } footer: {
                Text("The Octo/WuKongIM server and this bot's token.")
                    .font(.caption).foregroundStyle(.secondary)
            }

            Section {
                textRow("Model", prompt: "claude-opus-4-8", $bot.model)
                textRow("Gateway Base URL", prompt: "https://… (optional)", $bot.gatewayBaseURL, validateURL: true)
                secureRow("Gateway Token", prompt: "optional", $bot.gatewayToken)
            } header: {
                Label("Agent", systemImage: "cpu")
            } footer: {
                Text("Model + optional model-gateway, injected as ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN.")
                    .font(.caption).foregroundStyle(.secondary)
            }

            Section {
                promptEditor($bot.soul, placeholder: "Who this bot is — identity, voice, role.")
            } header: {
                Label("Persona — SOUL.md", systemImage: "sparkles")
            }

            Section {
                promptEditor($bot.agents, placeholder: "How it should behave — norms, do's and don'ts.")
            } header: {
                Label("Behavior — AGENTS.md", systemImage: "list.bullet.rectangle")
            } footer: {
                Text("Both are operator-trusted and prepended to the agent's system prompt. Leave empty to omit.")
                    .font(.caption).foregroundStyle(.secondary)
            }

            Section {
                EnvEditor(env: $bot.env)
            } header: {
                Label("Environment", systemImage: "terminal")
            } footer: {
                Text("Extra variables for the agent CLI (e.g. OCTO_BOT_ID, GH_TOKEN, GLAB_TOKEN).")
                    .font(.caption).foregroundStyle(.secondary)
            }

            Section {
                Button(role: .destructive) { onRemove() } label: {
                    Label("Remove Bot", systemImage: "trash")
                }
            }
        }
        .formStyle(.grouped)
    }

    // A labeled, full-width text field (label on top) — reads better than the
    // grouped-Form default that crams long URLs/tokens to the trailing edge.
    @ViewBuilder
    private func textRow(_ label: String, prompt: String, _ text: Binding<String>,
                         validateURL: Bool = false, help: String? = nil) -> some View {
        VStack(alignment: .leading, spacing: 5) {
            Text(label).font(.caption.weight(.medium)).foregroundStyle(.secondary)
            // Empty title + `prompt:` keeps the placeholder INSIDE the field;
            // `.labelsHidden()` stops the grouped Form from pulling a title into
            // a leading column (which crammed the value to the trailing edge).
            TextField("", text: text, prompt: Text(prompt))
                .textFieldStyle(.roundedBorder)
                .labelsHidden()
                .help(help ?? "")
            if validateURL, let w = urlWarning(text.wrappedValue) { warning(w) }
        }
        .padding(.vertical, 2)
    }

    @ViewBuilder
    private func secureRow(_ label: String, prompt: String, _ text: Binding<String>,
                           help: String? = nil) -> some View {
        VStack(alignment: .leading, spacing: 5) {
            Text(label).font(.caption.weight(.medium)).foregroundStyle(.secondary)
            RevealableSecureField(prompt, text: text)
                .help(help ?? "")
        }
        .padding(.vertical, 2)
    }

    private func warning(_ text: String) -> some View {
        Label(text, systemImage: "exclamationmark.triangle.fill")
            .font(.caption).foregroundStyle(.orange)
    }

    private func promptEditor(_ text: Binding<String>, placeholder: String) -> some View {
        ZStack(alignment: .topLeading) {
            if text.wrappedValue.isEmpty {
                Text(placeholder)
                    .font(.callout).foregroundStyle(.tertiary)
                    .padding(.horizontal, 5).padding(.vertical, 8)
                    .allowsHitTesting(false)
            }
            TextEditor(text: text)
                .font(.callout)
                .frame(minHeight: 72)
                .scrollContentBackground(.hidden)
        }
    }
}

/// Validates a URL against the core's SSRF policy: https anywhere, or
/// http://localhost. Returns a warning string for non-empty invalid input.
private func urlWarning(_ s: String) -> String? {
    guard !s.isEmpty else { return nil }
    guard let u = URL(string: s), let scheme = u.scheme?.lowercased(),
          let host = u.host, !host.isEmpty else {
        return "Not a valid URL"
    }
    if scheme == "https" { return nil }
    if scheme == "http", host == "localhost" || host == "127.0.0.1" || host == "::1" { return nil }
    return "Use https:// (or http://localhost)"
}

/// A secure text field with a reveal (eye) toggle.
private struct RevealableSecureField: View {
    let title: String
    @Binding var text: String
    @State private var revealed = false

    init(_ title: String, text: Binding<String>) {
        self.title = title
        self._text = text
    }

    var body: some View {
        HStack(spacing: 6) {
            Group {
                if revealed {
                    TextField("", text: $text, prompt: Text(title))
                } else {
                    SecureField("", text: $text, prompt: Text(title))
                }
            }
            .textFieldStyle(.roundedBorder)
            .labelsHidden()
            Button { revealed.toggle() } label: {
                Image(systemName: revealed ? "eye.slash" : "eye")
            }
            .buttonStyle(.borderless)
            .foregroundStyle(.secondary)
            .help(revealed ? "Hide" : "Reveal")
            .accessibilityLabel(revealed ? "Hide value" : "Reveal value")
        }
    }
}

/// Editable key/value list for per-bot environment variables. Backed by a
/// [String:String]; rendered as an ordered list with add/remove.
private struct EnvEditor: View {
    @Binding var env: [String: String]
    @State private var rows: [Row] = []

    struct Row: Identifiable { let id = UUID(); var key: String; var value: String }

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            ForEach($rows) { $row in
                HStack(spacing: 6) {
                    TextField("KEY", text: $row.key)
                        .labelsHidden()
                        .font(.system(.body, design: .monospaced))
                        .frame(minWidth: 120, maxWidth: 200)
                        .onChange(of: row.key) { sync() }
                    Text("=").foregroundStyle(.secondary)
                    SecureField("value", text: $row.value)
                        .labelsHidden()
                        .onChange(of: row.value) { sync() }
                    Button(role: .destructive) {
                        rows.removeAll { $0.id == row.id }
                        sync()
                    } label: { Image(systemName: "minus.circle") }
                    .buttonStyle(.borderless)
                    .accessibilityLabel("Remove variable")
                }
            }
            Button {
                rows.append(Row(key: "", value: ""))
            } label: { Label("Add variable", systemImage: "plus") }
            .buttonStyle(.borderless)
        }
        .onAppear { loadRows() }
        .onChange(of: env) { loadRowsIfExternallyChanged() }
    }

    private func loadRows() {
        if rows.isEmpty {
            rows = env.sorted { $0.key < $1.key }.map { Row(key: $0.key, value: $0.value) }
        }
    }

    private func loadRowsIfExternallyChanged() {
        let fromRows = Dictionary(rows.filter { !$0.key.isEmpty }.map { ($0.key, $0.value) }, uniquingKeysWith: { _, b in b })
        if fromRows != env {
            rows = env.sorted { $0.key < $1.key }.map { Row(key: $0.key, value: $0.value) }
        }
    }

    private func sync() {
        env = Dictionary(rows.filter { !$0.key.isEmpty }.map { ($0.key, $0.value) }, uniquingKeysWith: { _, b in b })
    }
}
