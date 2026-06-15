import { Events } from "@wailsio/runtime";
import { XClawService } from "../../bindings/github.com/lml2468/xclaw/desktop";

// The uid the desktop console talks to its bot under (a DM peer). The daemon
// assigns the real sessionKey; we adopt it from the first reply (see below).
const CONSOLE_UID = "gui-user";

export type Role = "user" | "assistant" | "tool";

export interface Message {
  id: string;
  role: Role;
  text: string;
  ts: number;
  streaming: boolean;
}

export interface Session {
  botId: string;
  key: string;        // sessionKey, or "pending:<bot>" before adoption
  title: string;
  messages: Message[];
  awaiting: boolean;  // a turn is in flight (show typing indicator)
  inputTokens: number;
  outputTokens: number;
  lastActivity: number;
}

export interface Bot {
  id: string;
  connected: boolean;
  lastError?: string;
}

interface Envelope {
  v: number; kind: string; id?: string; type: string; ts?: number; body?: any;
}

let uid = 0;
const newId = () => `m${++uid}`;

function prettyTitle(key: string): string {
  if (key.startsWith("pending:")) return "Console";
  const parts = key.split(":");
  if (parts.length > 1) return `${parts[0][0].toUpperCase()}${parts[0].slice(1)} · ${parts[parts.length - 1]}`;
  return key || "Console";
}

class Store {
  bots = $state<Bot[]>([]);
  sessions = $state<Session[]>([]);
  selectedBotId = $state<string | null>(null);
  selectedKey = $state<string | null>(null);
  health = $state("");
  lastError = $state("");
  connected = $state(false);

  // Per-bot adopted console sessionKey, and whether a console send is awaiting it.
  private consoleKey: Record<string, string> = {};
  private awaitingAdopt: Record<string, boolean> = {};

  constructor() {
    const params = new URLSearchParams(location.search);
    const theme = params.get("theme");
    if (theme === "dark" || theme === "light") document.documentElement.dataset.theme = theme;

    if (params.has("preview")) {
      this.seedPreview();
      return;
    }
    Events.On("xclaw:event", (e: any) => this.fold(e.data as Envelope));
    // Prime.
    XClawService.Health();
    XClawService.BotsList();
  }

  // seedPreview populates a mock roster + transcript for visual iteration and
  // screenshots without spawning the daemon (launch with XCLAW_PREVIEW=1).
  private seedPreview() {
    this.bots = [
      { id: "main", connected: true },
      { id: "research", connected: false, lastError: "awaiting secret" },
    ];
    this.health = "claude · 2 bots";
    this.connected = true;
    this.selectedBotId = "main";

    // `?empty` shows the empty state (no messages) for layout work.
    if (new URLSearchParams(location.search).has("empty")) {
      this.selectedKey = null;
      return;
    }
    const s: Session = {
      botId: "main", key: "console", title: "Console", awaiting: true,
      inputTokens: 1450, outputTokens: 92, lastActivity: Date.now(), messages: [
        { id: newId(), role: "user", text: "List the files in the project root and summarize what this repo does.", ts: 0, streaming: false },
        { id: newId(), role: "assistant", text: "I'll check the directory layout first.", ts: 0, streaming: false },
        { id: newId(), role: "tool", text: "Bash(ls -la)", ts: 0, streaming: false },
        { id: newId(), role: "assistant", text: "It's a **Go + Svelte** monorepo:\n\n- `core/` — the `xclawd` gateway daemon\n- `desktop/` — this Wails app\n- `proto/` — the control-bus contract\n\n```go\nfunc main() {\n    app.Run()\n}\n```\n\nWant me to open the README?", ts: 0, streaming: false },
        { id: newId(), role: "user", text: "yes, and the proto contract too", ts: 0, streaming: false },
        { id: newId(), role: "assistant", text: "Sure — the proto contract is an NDJSON envelope over a Unix socket: events out, commands in. Let me pull the", ts: 0, streaming: true },
      ],
    };
    this.sessions = [s];
    this.selectedBotId = "main";
    this.selectedKey = "console";
  }
  // --- derived selectors ---

  get currentBot(): Bot | null {
    return this.bots.find((b) => b.id === this.selectedBotId) ?? null;
  }
  get botSessions(): Session[] {
    return this.sessions
      .filter((s) => s.botId === this.selectedBotId)
      .sort((a, b) => b.lastActivity - a.lastActivity);
  }
  get currentSession(): Session | null {
    return this.sessions.find((s) => s.botId === this.selectedBotId && s.key === this.selectedKey) ?? null;
  }

  // --- commands ---

  selectBot(id: string) {
    this.selectedBotId = id;
    const sessions = this.botSessions;
    this.selectedKey = sessions[0]?.key ?? this.consoleKey[id] ?? null;
  }

  selectSession(key: string) {
    this.selectedKey = key;
  }

  send(text: string) {
    const botId = this.selectedBotId;
    if (!botId || !text.trim()) return;
    const key = this.consoleKey[botId] ?? `pending:${botId}`;
    const s = this.ensureSession(botId, key);
    s.messages.push({ id: newId(), role: "user", text, ts: Date.now() / 1000, streaming: false });
    s.awaiting = true;
    s.lastActivity = Date.now();
    if (!this.consoleKey[botId]) this.awaitingAdopt[botId] = true;
    this.selectedKey = key;
    XClawService.Send(botId, CONSOLE_UID, text);
  }

  reset() {
    const botId = this.selectedBotId;
    if (!botId) return;
    XClawService.Reset(botId, CONSOLE_UID);
    const s = this.currentSession;
    if (s) s.messages = [];
  }

  restartCore() {
    XClawService.RestartCore();
  }

  // --- event folding ---

  private fold(env: Envelope) {
    if (env.kind === "response") {
      if (env.type === "health" && env.body) {
        this.health = `${env.body.driver} · ${env.body.bots} bot${env.body.bots === 1 ? "" : "s"}`;
        this.connected = true;
      } else if (env.type === "bots.list" && Array.isArray(env.body)) {
        this.bots = env.body.map((b: any) => ({ id: b.id, connected: !!b.connected, lastError: b.lastError }));
        if (!this.selectedBotId && this.bots.length) this.selectBot(this.bots[0].id);
      } else if (env.type === "session.history" && Array.isArray(env.body)) {
        this.applyHistory(env.body);
      }
      return;
    }
    if (env.kind !== "event") return;

    switch (env.type) {
      case "bot.status": {
        const b = env.body;
        const found = this.bots.find((x) => x.id === b.id);
        if (found) { found.connected = !!b.connected; found.lastError = b.lastError; }
        else this.bots.push({ id: b.id, connected: !!b.connected, lastError: b.lastError });
        if (!this.selectedBotId) this.selectBot(b.id);
        break;
      }
      case "session.activity": {
        const s = this.route(env);
        if (!s) break;
        if (env.body.kind === "turnStart") s.awaiting = true;
        if (env.body.kind === "turnDone") { s.awaiting = false; this.finalizeStreaming(s); }
        break;
      }
      case "session.text": {
        const s = this.route(env);
        if (!s) break;
        s.awaiting = false;
        let m = s.messages.find((x) => x.streaming && x.role === "assistant");
        if (!m) { m = { id: newId(), role: "assistant", text: "", ts: Date.now() / 1000, streaming: true }; s.messages.push(m); }
        m.text += env.body.delta ?? "";
        s.lastActivity = Date.now();
        break;
      }
      case "session.tool": {
        const s = this.route(env);
        if (!s) break;
        this.finalizeStreaming(s);
        s.messages.push({ id: newId(), role: "tool", text: `${env.body.name}(${env.body.params ?? ""})`, ts: Date.now() / 1000, streaming: false });
        break;
      }
      case "session.reply": {
        const s = this.route(env);
        if (!s) break;
        s.awaiting = false;
        // The reply is the final assembled text. If the trailing message is the
        // assistant turn we just streamed (whether or not turnDone already cleared
        // its streaming flag), update it in place; only append when there's no
        // assistant bubble to finalize (e.g. a turn that streamed no text deltas).
        const last = s.messages[s.messages.length - 1];
        if (last && last.role === "assistant") {
          last.text = env.body.text ?? last.text;
          last.streaming = false;
        } else if (env.body.text) {
          s.messages.push({ id: newId(), role: "assistant", text: env.body.text, ts: Date.now() / 1000, streaming: false });
        }
        break;
      }
      case "session.usage": {
        const s = this.route(env);
        if (!s) break;
        s.inputTokens = env.body.inputTokens ?? 0;
        s.outputTokens = env.body.outputTokens ?? 0;
        break;
      }
      case "error": {
        this.lastError = env.body?.message ?? "error";
        break;
      }
    }
  }

  private finalizeStreaming(s: Session) {
    for (const m of s.messages) if (m.streaming) m.streaming = false;
  }

  // route resolves the session an event belongs to, adopting the console key
  // from the first reply after a console send.
  private route(env: Envelope): Session | null {
    const botId = env.body?.botId || this.defaultBotId();
    const key = env.body?.sessionKey ?? "";
    if (!botId) return null;

    if (this.awaitingAdopt[botId] && !this.consoleKey[botId] && key) {
      const pending = this.sessions.find((s) => s.botId === botId && s.key === `pending:${botId}`);
      if (pending) {
        pending.key = key;
        pending.title = prettyTitle(key);
        if (this.selectedKey === `pending:${botId}`) this.selectedKey = key;
      }
      this.consoleKey[botId] = key;
      this.awaitingAdopt[botId] = false;
    }
    return this.ensureSession(botId, key || `pending:${botId}`);
  }

  private defaultBotId(): string {
    return this.selectedBotId ?? this.bots[0]?.id ?? "";
  }

  private ensureSession(botId: string, key: string): Session {
    let s = this.sessions.find((x) => x.botId === botId && x.key === key);
    if (!s) {
      s = { botId, key, title: prettyTitle(key), messages: [], awaiting: false, inputTokens: 0, outputTokens: 0, lastActivity: Date.now() };
      this.sessions.push(s);
    }
    // Surface an arriving conversation when nothing is selected yet (first
    // connect, incoming IM, or an externally-driven turn) so the transcript
    // isn't left on the empty state while messages stream into the list.
    if (!this.selectedBotId) this.selectedBotId = botId;
    if (this.selectedKey == null && botId === this.selectedBotId) this.selectedKey = key;
    return s;
  }

  private applyHistory(rows: any[]) {
    const s = this.currentSession;
    if (!s || s.messages.length) return;
    s.messages = rows.map((r) => ({ id: newId(), role: r.role as Role, text: r.content, ts: r.ts, streaming: false }));
  }
}

export const store = new Store();
