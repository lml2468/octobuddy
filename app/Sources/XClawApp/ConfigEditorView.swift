import SwiftUI
import XClawCore

/// Bot configuration editor (opened via Settings / Cmd-,). Lists configured
/// bots, lets you add/remove and edit id / apiUrl / tokens, and saves to
/// ~/.xclaw/config.json. Tokens are stored in the macOS Keychain (not the file);
/// see AppModel.saveConfig / Keychain.swift.
struct ConfigEditorView: View {
    @Bindable var model: AppModel
    @State private var selection: String?

    var body: some View {
        NavigationSplitView {
            List(selection: $selection) {
                Section("Bots") {
                    ForEach($model.configBots) { $bot in
                        Text(bot.id.isEmpty ? "(unnamed)" : bot.id).tag(bot.id)
                    }
                    .onDelete { idx in
                        let ids = idx.map { model.configBots[$0].id }
                        ids.forEach { model.removeConfigBot($0) }
                    }
                }
            }
            .frame(minWidth: 180)
            .toolbar {
                ToolbarItem {
                    Button {
                        model.addConfigBot()
                        selection = model.configBots.last?.id
                    } label: { Image(systemName: "plus") }
                }
            }
        } detail: {
            if let sel = selection, let i = model.configBots.firstIndex(where: { $0.id == sel }) {
                BotForm(bot: $model.configBots[i])
                    .id(sel) // rebuild cleanly when switching bots
            } else {
                ContentUnavailableView(
                    "No Bot Selected",
                    systemImage: "bolt.horizontal.circle",
                    description: Text("Select a bot, or add one with +.")
                )
            }
        }
        .frame(width: 620, height: 420)
        .safeAreaInset(edge: .bottom) { footer }
    }

    private var footer: some View {
        HStack {
            if let err = model.configError {
                Label(err, systemImage: "exclamationmark.triangle")
                    .font(.caption).foregroundStyle(.red).lineLimit(2)
            } else if model.needsRestart {
                Label("Saved. Restart the core to apply.", systemImage: "checkmark.circle")
                    .font(.caption).foregroundStyle(.secondary)
            }
            Spacer()
            Button("Save") { model.saveConfig() }
                .keyboardShortcut("s", modifiers: .command)
            Button("Save & Restart") {
                if model.saveConfig() { model.applyConfigAndRestart() }
            }
            .buttonStyle(.borderedProminent)
        }
        .padding(12)
        .background(.bar)
    }
}

private struct BotForm: View {
    @Binding var bot: BotConfig

    var body: some View {
        Form {
            Section("Identity") {
                TextField("Bot ID", text: $bot.id)
                    .help("Letters, digits, dot, underscore, hyphen. Used as the subtree name under ~/.xclaw.")
                if !ConfigStore.validSlug(bot.id) {
                    Text("Invalid id — letters/digits/._- only")
                        .font(.caption).foregroundStyle(.red)
                }
            }
            Section("Octo") {
                TextField("API URL", text: $bot.apiURL)
                    .textContentType(.URL)
                SecureField("Bot Token (bf_…)", text: $bot.octoToken)
                    .help("Stored in your macOS Keychain, not in config.json.")
            }
            Section {
                TextField("Gateway Base URL", text: $bot.gatewayBaseURL)
                    .textContentType(.URL)
                SecureField("Gateway Token", text: $bot.gatewayToken)
            } header: {
                Text("Model Gateway")
            } footer: {
                Text("Injected as ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN.")
                    .font(.caption)
            }
            Section {
                EnvEditor(env: $bot.env)
            } header: {
                Text("Environment")
            } footer: {
                Text("Extra variables for the agent CLI (e.g. OCTO_BOT_ID, GH_TOKEN, GLAB_TOKEN).")
                    .font(.caption)
            }
        }
        .formStyle(.grouped)
        .padding()
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
                        .font(.system(.body, design: .monospaced))
                        .frame(maxWidth: 180)
                        .onChange(of: row.key) { sync() }
                    Text("=").foregroundStyle(.secondary)
                    SecureField("value", text: $row.value)
                        .onChange(of: row.value) { sync() }
                    Button(role: .destructive) {
                        rows.removeAll { $0.id == row.id }
                        sync()
                    } label: { Image(systemName: "minus.circle") }
                    .buttonStyle(.borderless)
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

    // Reload rows only when the underlying dict changed from outside (e.g. bot
    // switch), not from our own edits.
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
