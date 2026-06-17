import { Events } from "@wailsio/runtime";
import { XClawService } from "../../bindings/github.com/lml2468/xclaw/desktop";

// The uid the desktop console talks to its bot under (a DM peer). Control-bus
// sends carry no space, so the daemon derives the session key as exactly this
// uid — i.e. the console session key is deterministic. We key the console
// session on it directly rather than adopting a key from the reply stream,
// which a concurrent IM turn could otherwise hijack.
const CONSOLE_UID = "gui-user";

export type Role = "user" | "assistant" | "tool";

export interface Message {
  id: string;
  role: Role;
  text: string;
  ts: number;
  streaming: boolean;
}

// ProcStep is one process item shown in the status box: a tool call, a thinking
// marker, or a completed narration block (intermediate prose the agent wrote
// before a tool call). None of these are the final answer — they are process.
export interface ProcStep {
  id: string;
  kind: "tool" | "thinking" | "text";
  text: string;
}

// ProcState is the live, in-flight process for a session's current turn.
//
// The backend gives us no "this is the final answer" flag — every prose chunk is
// an identical session.text delta, and the only structural signal that a text
// block ended is a session.tool event (or turn end). So we treat each text block
// as PROCESS until proven final: deltas accumulate in `live`; when a tool arrives
// it flushes `live` into a completed narration step. Whatever remains in `live`
// at session.reply is the block after the last tool = the final answer, which
// drops into the chat. Nothing in this state is the final answer — it is cleared
// on session.reply, so the status box only ever shows process.
export interface ProcState {
  steps: ProcStep[];   // completed process items this turn (tools, thinking, flushed narration)
  live: string;        // current text block, still accumulating (NOT rendered; buffer only)
  active: boolean;     // a turn is in flight
}

const emptyProc = (): ProcState => ({ steps: [], live: "", active: false });

export interface Session {
  botId: string;
  key: string;        // sessionKey (the console session is keyed on CONSOLE_UID)
  title: string;
  messages: Message[];
  awaiting: boolean;  // a turn is in flight (show typing indicator)
  proc: ProcState;    // live process info for the status box (not in the transcript)
  inputTokens: number;
  outputTokens: number;
  cachedInputTokens: number;
  costUsd: number;
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
  if (key === CONSOLE_UID || key === "console") return "Console";
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
      proc: {
        active: true,
        // Process for the in-flight turn: a narration block, a thinking marker,
        // and tool calls. The answer-in-progress sits in `live` (buffered, not
        // rendered) — the chat shows the working spinner until session.reply.
        steps: [
          { id: newId(), kind: "text", text: "I'll check the directory layout first, then read the proto contract." },
          { id: newId(), kind: "tool", text: "Bash(ls -la)" },
          { id: newId(), kind: "thinking", text: "thinking…" },
          { id: newId(), kind: "tool", text: "Read(proto/README.md)" },
        ],
        live: "Sure — the proto contract is an NDJSON envelope over a Unix socket: events out, commands in. Let me pull the",
      },
      inputTokens: 1450, outputTokens: 92, cachedInputTokens: 1200, costUsd: 0.0123, lastActivity: Date.now(), messages: [
        { id: newId(), role: "user", text: "List the files in the project root and summarize what this repo does.", ts: 0, streaming: false },
        { id: newId(), role: "assistant", text: "It's a **Go + Svelte** monorepo:\n\n- `core/` — the `xclawd` gateway daemon\n- `desktop/` — this Wails app\n- `proto/` — the control-bus contract\n\n```go\nfunc main() {\n    app.Run()\n}\n```\n\nWant me to open the README?", ts: 0, streaming: false },
        { id: newId(), role: "user", text: "yes, and the proto contract too", ts: 0, streaming: false },
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
    this.selectedKey = sessions[0]?.key ?? null;
  }

  selectSession(key: string) {
    this.selectedKey = key;
  }

  send(text: string) {
    const botId = this.selectedBotId;
    if (!botId || !text.trim()) return;
    // The console session key is deterministic (control-bus DM → key == CONSOLE_UID),
    // so use it directly — no "adopt the first reply's key" guesswork.
    const key = CONSOLE_UID;
    const s = this.ensureSession(botId, key);
    s.messages.push({ id: newId(), role: "user", text, ts: Date.now() / 1000, streaming: false });
    s.awaiting = true;
    s.lastActivity = Date.now();
    this.selectedKey = key;
    XClawService.Send(botId, CONSOLE_UID, text);
  }

  reset() {
    const botId = this.selectedBotId;
    if (!botId) return;
    XClawService.Reset(botId, CONSOLE_UID);
    const s = this.currentSession;
    if (s) { s.messages = []; s.proc = emptyProc(); }
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
        if (env.body.kind === "turnStart") { s.awaiting = true; s.proc = emptyProc(); s.proc.active = true; }
        else if (env.body.kind === "thinking") {
          // Thinking is process → status box. Flush any pending narration first so
          // ordering reads correctly, then add the marker (no consecutive spam).
          this.flushLive(s);
          s.proc.active = true;
          const last = s.proc.steps[s.proc.steps.length - 1];
          if (!last || last.kind !== "thinking") s.proc.steps.push({ id: newId(), kind: "thinking", text: "thinking…" });
        }
        // turnDone just clears the typing affordance; session.reply (next) is the
        // single point that promotes the final block into the chat.
        else if (env.body.kind === "turnDone") s.awaiting = false;
        break;
      }
      case "session.text": {
        const s = this.route(env);
        if (!s) break;
        // Accumulate the current text block in the process buffer. We can't yet
        // tell if it's narration or the final answer — a following tool would make
        // it narration; reaching session.reply makes it the answer. It is NOT
        // rendered (the chat shows the working spinner), so nothing leaks.
        s.proc.active = true;
        s.proc.live += env.body.delta ?? "";
        s.lastActivity = Date.now();
        break;
      }
      case "session.tool": {
        const s = this.route(env);
        if (!s) break;
        // A tool call proves the current text block was intermediate narration:
        // flush it to a completed step, then record the tool. proc.live restarts.
        this.flushLive(s);
        s.proc.active = true;
        s.proc.steps.push({ id: newId(), kind: "tool", text: `${env.body.name}(${env.body.params ?? ""})` });
        s.lastActivity = Date.now();
        break;
      }
      case "session.reply": {
        const s = this.route(env);
        if (!s) break;
        // The single finalize point. The final answer is the block still in
        // proc.live (text after the last tool); fall back to the server's full
        // assembled text only when nothing streamed. It enters the chat now —
        // the first and only time response text touches the transcript — then the
        // status box clears so no process survives the turn.
        const final = (s.proc.live.trim() ? s.proc.live : (env.body.text ?? "")).trim();
        if (final) s.messages.push({ id: newId(), role: "assistant", text: final, ts: Date.now() / 1000, streaming: false });
        s.awaiting = false;
        s.proc = emptyProc();   // clear the status box
        s.lastActivity = Date.now();
        break;
      }
      case "session.usage": {
        const s = this.route(env);
        if (!s) break;
        s.inputTokens = env.body.inputTokens ?? 0;
        s.outputTokens = env.body.outputTokens ?? 0;
        s.cachedInputTokens = env.body.cachedInputTokens ?? 0;
        s.costUsd = env.body.costUsd ?? 0;
        break;
      }
      case "error": {
        this.lastError = env.body?.message ?? "error";
        break;
      }
    }
  }

  // flushLive moves the accumulating text block (proc.live) into a completed
  // narration step in the status box. Called when a tool/thinking event proves
  // the block was intermediate process, not the final answer. No-op if empty.
  private flushLive(s: Session) {
    const t = s.proc.live.trim();
    if (t) s.proc.steps.push({ id: newId(), kind: "text", text: t });
    s.proc.live = "";
  }

  // route resolves the session an event belongs to. The console session is
  // keyed on CONSOLE_UID; IM-originated turns carry their own sessionKey and get
  // their own row. No key adoption (which a concurrent IM turn could hijack).
  private route(env: Envelope): Session | null {
    const botId = env.body?.botId || this.defaultBotId();
    const key = env.body?.sessionKey ?? "";
    if (!botId) return null;
    return this.ensureSession(botId, key || CONSOLE_UID);
  }

  private defaultBotId(): string {
    return this.selectedBotId ?? this.bots[0]?.id ?? "";
  }

  private ensureSession(botId: string, key: string): Session {
    let s = this.sessions.find((x) => x.botId === botId && x.key === key);
    if (!s) {
      s = { botId, key, title: prettyTitle(key), messages: [], awaiting: false, proc: emptyProc(), inputTokens: 0, outputTokens: 0, cachedInputTokens: 0, costUsd: 0, lastActivity: Date.now() };
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
