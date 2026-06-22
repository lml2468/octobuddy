import { Events } from "@wailsio/runtime";
import { XClawService } from "../../bindings/github.com/lml2468/xclaw/desktop";

// The uid the desktop console talks to its bot under (a DM peer). Control-bus
// sends carry no space, so the daemon derives the session key as exactly this
// uid — i.e. the console session key is deterministic. We key the console
// session on it directly rather than adopting a key from the reply stream,
// which a concurrent IM turn could otherwise hijack.
const CONSOLE_UID = "gui-user";

// TURN_MAX_MS caps how long a session may sit with awaiting=true with no
// terminal event before the sweeper clears it. Must stay STRICTLY GREATER
// than the daemon's `defaultDispatchTimeout` (currently 20 min, an IDLE
// deadline — reset on every AgentEvent) plus reconnect slack; otherwise a
// healthy multi-tool turn that streams >8 min would trigger a false-positive
// "长时间未收到响应" while the reply is still pending. The two values are
// intentionally not auto-derived (no env-config channel for an FE constant);
// keep them in sync by hand and review on any daemon-side timeout change.
// TURN_SWEEP_MS is the poll cadence.
const TURN_MAX_MS = 22 * 60 * 1000;
const TURN_SWEEP_MS = 30 * 1000;

export type Role = "user" | "assistant" | "tool";

export interface Message {
  id: string;
  role: Role;
  text: string;
  ts: number;
}

// ProcStep is one process item shown in the status strip: a tool call or a
// thinking marker. It is NEVER model prose — the final answer comes from the
// backend's authoritative session.reply.text, so the strip can show only
// unambiguous "process" and answer-leakage is structurally impossible.
export interface ProcStep {
  id: string;
  kind: "tool" | "thinking";
  text: string;
}

// ProcState is the live, in-flight process for a session's current turn — the
// tools/thinking the agent is doing right now. The final answer is NOT tracked
// here: the gateway sends it whole in session.reply (the same assembled text it
// persists), so we never reconstruct it from text deltas. Cleared on reply.
export interface ProcState {
  steps: ProcStep[];   // process items this turn (tools, thinking markers)
  active: boolean;     // a turn is in flight
}

const emptyProc = (): ProcState => ({ steps: [], active: false });

export interface Session {
  botId: string;
  key: string;        // sessionKey (the console session is keyed on CONSOLE_UID)
  title: string;
  messages: Message[];
  awaiting: boolean;  // a turn is in flight (show typing indicator)
 // awaitingSince is the Date.now of the most recent turnStart for this
 // session; a max-age sweep clears `awaiting` (and surfaces a synthetic
 // error) when no terminal event has arrived in TURN_MAX_MS. Without this,
 // a daemon crash / control-socket drop mid-turn left the typing indicator
 // hanging forever and the only escape was sending another message.
  awaitingSince: number;
  proc: ProcState;    // live process info for the status box (not in the transcript)
  inputTokens: number;
  outputTokens: number;
  cachedInputTokens: number;
  costUsd: number;
  lastActivity: number;
  preview?: string;   // last-message preview for the list (from sessions.list, before messages load)
  loaded?: boolean;   // true once full history has been fetched, so we don't refetch on every open
 // historyEpoch is bumped on every reset() so a slow History response that
 // started before the reset can be dropped on arrival instead of restoring
 // the rows the operator just cleared. expectedHistoryEpoch is the value
 // captured when loadHistory issued the in-flight request.
  historyEpoch?: number;
  expectedHistoryEpoch?: number;
}

export interface Bot {
  id: string;
  connected: boolean;
  lastError?: string;
 // Token usage keyed by range bound (`since` Unix seconds; 0 = all time), so
 // switching ranges in the Token Usage window doesn't clobber other ranges.
  usage?: Record<number, BotUsage>;
}

// BotUsage is a bot's token consumption over one range (persisted, per bot).
export interface BotUsage {
  inputTokens: number;
  outputTokens: number;
  cachedTokens: number;     // cache reads
  cacheWriteTokens: number; // cache writes (seeding the prompt cache)
  costUsd: number;
  turns: number;
}

interface Envelope {
  v: number; kind: string; id?: string; type: string; ts?: number; body?: any;
}

let uid = 0;
const newId = () => `m${++uid}`;

function prettyTitle(key: string): string {
  if (key === CONSOLE_UID || key === "console") return "Console";
  const parts = key.split(":");
 // Guard against an empty first segment (e.g. a ":uid" key): parts[0][0] would
 // throw and break the whole reducer fold. Fall back gracefully.
  if (parts.length > 1 && parts[0]) {
    return `${parts[0][0].toUpperCase()}${parts[0].slice(1)} · ${parts[parts.length - 1]}`;
  }
  return key || "Console";
}

class Store {
  bots = $state<Bot[]>([]);
  sessions = $state<Session[]>([]);
  selectedBotId = $state<string | null>(null);
  selectedKey = $state<string | null>(null);
  health = $state("");
  lastError = $state("");
 // Track the exact text the user dismissed via the ✕ button so the
 // reconnect storm (bridge.status events firing every retry while the
 // daemon is down) doesn't re-pin the same banner on the next tick.
 // the prior ✕ cleared once; the next bridge.status with
 // an identical detail unconditionally re-wrote lastError. Tracking the
 // last-dismissed text + suppressing duplicate writes makes the dismiss
 // sticky for the lifetime of that error condition. A different detail
 // (e.g. daemon comes back, then a new failure) clears the suppression.
  private dismissedError = $state("");

 // clearLastError dismisses the global error banner. Bound to Transcript's
 // ✕ button so a transient envelope failure doesn't pin a red bar above
 // the chat for the rest of the session.
  clearLastError() {
    this.dismissedError = this.lastError;
    this.lastError = "";
  }
 // setError centralizes lastError writes so we can suppress a repeat of
 // a just-dismissed text.
  private setError(text: string) {
    if (text === this.dismissedError) return;
    this.lastError = text;
  }
  connected = $state(false);
 // True in preview mode (XCLAW_PREVIEW / ?preview): seeded mock data, no daemon,
 // so we skip the control-bus fetches (SessionsList/History).
  readonly preview = new URLSearchParams(location.search).has("preview");

  constructor() {
    const params = new URLSearchParams(location.search);
    const theme = params.get("theme");
    if (theme === "dark" || theme === "light") document.documentElement.dataset.theme = theme;

    if (this.preview) {
      this.seedPreview();
      return;
    }
 // Wails Events.On returns an unsubscribe handle — capture it so HMR
 // dispose can actually unsubscribe (: the prior
 // dispose only cleared the setInterval, leaving xclaw:event listeners
 // to stack on every save and fold to fire N times per envelope).
 // Wrap fold in try/catch so a single malformed envelope can't kill
 // the bridge listener — bad data appears in lastError instead.
    this.unsubFold = Events.On("xclaw:event", (e: any) => {
      try {
        this.fold(e.data as Envelope);
      } catch (err) {
        const msg = err instanceof Error ? err.message : String(err);
        console.error("[store] fold failed", err);
        this.setError("[fold] " + msg);
      }
    });
 // Prime.
    XClawService.Health();
    XClawService.BotsList();
 // Stuck-turn sweeper: if no terminal event (turnDone / session.reply /
 // error) arrives within TURN_MAX_MS of a turnStart, clear `awaiting`
 // and surface a synthetic error. Without this, a daemon crash /
 // control-socket drop mid-turn left the typing indicator hanging
 // forever and the only escape was sending another message (which
 // simply flipped awaiting=true again, masking the dead turn).
    this.sweepTimer = setInterval(() => this.sweepStuckTurns(), TURN_SWEEP_MS);
  }

 // dispose runs from Vite's import.meta.hot.dispose so a dev save (HMR)
 // doesn't stack a fresh setInterval and a fresh xclaw:event subscription
 // on top of the prior module's still-armed ones. Production never calls
 // this — the singleton survives for the app's lifetime (F3,
 // unsub wired in).
  dispose() {
    if (this.sweepTimer !== undefined) {
      clearInterval(this.sweepTimer);
      this.sweepTimer = undefined;
    }
    if (this.unsubFold) {
      try { this.unsubFold(); } catch {}
      this.unsubFold = undefined;
    }
  }
  private sweepTimer: ReturnType<typeof setInterval> | undefined;
  private unsubFold: (() => void) | undefined;

 // seedPreview populates a mock roster + transcript for visual iteration and
 // screenshots without spawning the daemon (launch with XCLAW_PREVIEW=1).
  private seedPreview() {
 // Preview usage: keyed by `since` (0 = all). Smaller numbers for shorter
 // ranges so the selector visibly changes. Keys filled in below after we know
 // the range bounds the modal computes.
    const pv = (i: number, o: number, cr: number, cw: number, c: number, t: number): BotUsage =>
      ({ inputTokens: i, outputTokens: o, cachedTokens: cr, cacheWriteTokens: cw, costUsd: c, turns: t });
    this.bots = [
      { id: "main", connected: true, usage: { 0: pv(1_284_500, 96_120, 842_300, 318_400, 4.8123, 318) } },
      { id: "research", connected: false, lastError: "awaiting secret", usage: { 0: pv(412_900, 38_540, 201_770, 64_200, 1.2045, 92) } },
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
      botId: "main", key: "console", title: "Console", awaiting: true, awaitingSince: Date.now(),
      proc: {
        active: true,
 // Process for the in-flight turn: thinking + tool calls only. The
 // answer-in-progress is NOT here — the chat shows a working indicator
 // until the whole reply arrives in session.reply.
        steps: [
          { id: newId(), kind: "tool", text: "Bash(ls -la)" },
          { id: newId(), kind: "thinking", text: "thinking…" },
          { id: newId(), kind: "tool", text: "Read(proto/README.md)" },
        ],
      },
      inputTokens: 1450, outputTokens: 92, cachedInputTokens: 1200, costUsd: 0.0123, lastActivity: Date.now(), messages: [
        { id: newId(), role: "user", text: "List the files in the project root and summarize what this repo does.", ts: 0 },
        { id: newId(), role: "assistant", text: "It's a **Go + Svelte** monorepo:\n\n- `core/` — the `xclawd` gateway daemon\n- `desktop/` — this Wails app\n- `proto/` — the control-bus contract\n\n```go\nfunc main() {\n    app.Run()\n}\n```\n\nWant me to open the README?", ts: 0 },
        { id: newId(), role: "user", text: "yes, and the proto contract too", ts: 0 },
      ],
    };
    this.sessions = [s];
 // Extra history rows (preview-only, like a real sessions.list) so the denser
 // list renders several items for layout/screenshot work.
    const hist: Array<[string, string, string, number]> = [
      ["dm:alice", "Alice", "Sounds good — I'll push the fix tonight.", 6 * 60_000],
      ["group:eng", "Eng · #backend", "the migration is green on staging", 42 * 60_000],
      ["dm:bob", "Bob", "thanks! that unblocked me", 3 * 3600_000],
      ["dm:carol", "Carol", "can you review the PR when you get a sec?", 26 * 3600_000],
      ["group:ops", "Ops · #alerts", "resolved: latency back to baseline", 3 * 86400_000],
    ];
    for (const [key, title, prev, ago] of hist) {
      this.sessions.push({
        botId: "main", key, title, messages: [], awaiting: false, awaitingSince: 0, proc: emptyProc(),
        inputTokens: 0, outputTokens: 0, cachedInputTokens: 0, costUsd: 0,
        lastActivity: Date.now() - ago, preview: prev,
      });
    }
    this.selectedBotId = "main";
    this.selectedKey = "console";
  }
 // --- derived selectors ---
 // Each is a Svelte 5 $derived: recomputed once per dependency change and
 // cached for every read in between, instead of the `get`-accessor pattern
 // which re-runs the body on every component access.

  currentBot = $derived<Bot | null>(this.bots.find((b) => b.id === this.selectedBotId) ?? null);

  botSessions = $derived<Session[]>(
    this.sessions
      .filter((s) => s.botId === this.selectedBotId)
      .sort((a, b) => b.lastActivity - a.lastActivity),
  );

  currentSession = $derived<Session | null>(
    this.sessions.find((s) => s.botId === this.selectedBotId && s.key === this.selectedKey) ?? null,
  );

 // Only the Console session is writable from the desktop. Every other session
 // (DM / Group) originates from Octo IM — the human counterpart lives there, so
 // the desktop is an observation surface only. The Composer is hidden when this
 // is false; send additionally no-ops as defense in depth.
  isConsole = $derived<boolean>(this.selectedKey === CONSOLE_UID || this.selectedKey === "console");

 // --- commands ---

  selectBot(id: string) {
    this.selectedBotId = id;
 // Pull this bot's full persisted session list (newest first); the response
 // folds into sessions[] so history survives restarts.
    if (!this.preview) XClawService.SessionsList(id);
    this.selectedKey = this.initialKey();
    if (this.selectedKey) this.loadHistory(this.selectedKey);
  }

 // initialKey is the key to land on when first opening a bot: its newest
 // persisted session, or the Console for a fresh bot whose roster is empty
 // (so the chat lands on a writable surface instead of the IM-empty
 // "loading" fallback for a key that doesn't exist).
  private initialKey(): string {
    return this.botSessions[0]?.key ?? CONSOLE_UID;
  }

 // loadUsage fetches token usage for every bot over a range (since = Unix
 // seconds; 0 = all time). Responses fold into bot.usage[since]. Called by the
 // Token Usage window on open and whenever the range changes. The returned
 // Promise resolves once every per-bot UsageStats request has settled — note
 // allSettled (not all): a single failed bot must not blank every other
 // bot's number, and the modal's spinner must clear instead of hanging.
  loadUsage(since: number = 0): Promise<unknown[]> {
    if (this.preview) {
      this.seedUsageRange(since);
      return Promise.resolve([]);
    }
    return Promise.allSettled(this.bots.map((b) => XClawService.UsageStats(b.id, since)));
  }

 // Preview-only: synthesize a range's usage by scaling the all-time (since=0)
 // figures, so the date-range selector visibly changes in screenshots.
  private seedUsageRange(since: number) {
    if (since === 0) return; // all-time already seeded
    const days = Math.max(1, Math.round((Date.now() / 1000 - since) / 86400));
    const frac = Math.min(1, days / 365); // pretend ~1yr of history
    for (const b of this.bots) {
      const all = b.usage?.[0];
      if (!all) continue;
      const s = (n: number) => Math.round(n * frac);
      b.usage = { ...b.usage, [since]: {
        inputTokens: s(all.inputTokens), outputTokens: s(all.outputTokens),
        cachedTokens: s(all.cachedTokens), cacheWriteTokens: s(all.cacheWriteTokens),
        costUsd: all.costUsd * frac, turns: s(all.turns),
      } };
    }
  }

  selectSession(key: string) {
    this.selectedKey = key;
    this.loadHistory(key);
  }

 // loadHistory lazily fetches a session's transcript the first time it's opened
 // (sessions.list only carries a preview). applyHistory routes to currentSession,
 // so we fetch right after selecting; the loaded flag prevents refetching.
 // Console is included: the daemon persists its messages just like any other
 // session, so on app relaunch we must fetch them or the chat appears empty.
 //
 // Eagerly upserts a placeholder session row if missing so a cross-bot jump
 // from the CommandPalette (selectBot + selectSession synchronously into a
 // bot whose roster hasn't loaded yet) doesn't silently no-op the history
 // fetch. Without this, the transcript stayed blank until the user typed
 // anything to force a session row.
  private loadHistory(key: string) {
    if (this.preview) return;
    const botId = this.selectedBotId;
    if (!botId) return;
    let s = this.sessions.find((x) => x.botId === botId && x.key === key);
    if (!s) {
 // initialActivity=0 so the placeholder sinks to the bottom of the
 // lastActivity-sorted sidebar list until the eventual session.history
 // / sessions.list response folds the persisted updatedAt back in.
 // Without this, a CommandPalette cross-bot jump into a stale session
 // would float it permanently to the top (regression from).
      s = this.ensureSession(botId, key, 0);
    }
    if (s.loaded) return;
    s.expectedHistoryEpoch = s.historyEpoch ?? 0;
    XClawService.History(botId, key, 0);
  }

  send(text: string) {
    const botId = this.selectedBotId;
    if (!botId || !text.trim()) return;
 // Desktop only writes to the Console session. IM-originated sessions belong
 // to the remote human in DM/Group; the Composer is hidden for them, but
 // guard here too so a stray keybinding path can't inject as the bot.
    if (!this.isConsole && this.selectedKey != null) return;
    const key = CONSOLE_UID;
    const s = this.ensureSession(botId, key);
    s.messages.push({ id: newId(), role: "user", text, ts: Date.now() / 1000 });
    s.awaiting = true;
    s.awaitingSince = Date.now();
    s.lastActivity = Date.now();
    this.selectedKey = key;
    XClawService.Send(botId, CONSOLE_UID, text);
  }

 // Reset clears the resume id for the active session and wipes its visible
 // transcript. Works for any session (including IM-originated ones) — useful
 // when a resume id has gone stale and the operator wants the next IM turn to
 // start fresh. The IM-side human will not see this; they just notice the
 // bot's memory has been cleared on their next message.
  reset() {
    const botId = this.selectedBotId;
    const key = this.selectedKey;
    if (!botId || !key) return;
    XClawService.Reset(botId, key);
    const s = this.currentSession;
    if (s) {
      s.messages = [];
      s.proc = emptyProc();
 // Bump the epoch so any History response issued before this reset
 // (still in flight over the control bus) is dropped on arrival
 // instead of restoring the rows the operator just cleared. Also
 // set loaded=true so a subsequent loadHistory (e.g. from a reconnect's
 // applySessionsList tail-fetch, or a re-selection) does not auto-
 // restore the persisted rows the operator explicitly cleared. The
 // shared `expectedHistoryEpoch` field would otherwise be overwritten
 // by that second fetch, letting the still-in-flight first response
 // pass the guard. Operator wants it back → they reload by another
 // explicit action (not implemented today; out of scope).
      s.historyEpoch = (s.historyEpoch ?? 0) + 1;
      s.loaded = true;
    }
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
      } else if (env.type === "sessions.list" && env.body && Array.isArray(env.body.sessions)) {
        this.applySessionsList(env.body.botId, env.body.sessions);
      } else if (env.type === "usage.stats" && env.body) {
        const b = this.bots.find((x) => x.id === env.body.botId);
        if (b) {
          const since = env.body.since ?? 0;
          const u: BotUsage = {
            inputTokens: env.body.inputTokens ?? 0,
            outputTokens: env.body.outputTokens ?? 0,
            cachedTokens: env.body.cachedTokens ?? 0,
            cacheWriteTokens: env.body.cacheWriteTokens ?? 0,
            costUsd: env.body.costUsd ?? 0,
            turns: env.body.turns ?? 0,
          };
          b.usage = { ...(b.usage ?? {}), [since]: u };
        }
      } else if (env.type === "session.history" && env.body && Array.isArray(env.body.messages)) {
        this.applyHistory(env.body.botId, env.body.key, env.body.messages);
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
        if (env.body.kind === "turnStart") { s.awaiting = true; s.awaitingSince = Date.now(); s.proc = emptyProc(); s.proc.active = true; }
        else if (env.body.kind === "thinking") {
 // Thinking is process → status strip. Coalesce consecutive markers.
          s.proc.active = true;
          const last = s.proc.steps[s.proc.steps.length - 1];
          if (!last || last.kind !== "thinking") s.proc.steps.push({ id: newId(), kind: "thinking", text: "thinking…" });
        }
 // turnDone just clears the typing affordance; session.reply (next) is the
 // single point that delivers the answer into the chat.
        else if (env.body.kind === "turnDone") s.awaiting = false;
        break;
      }
      case "session.text": {
        const s = this.route(env);
        if (!s) break;
 // Model prose is NOT rendered mid-turn and NOT buffered: the chat shows a
 // working indicator, and the whole answer arrives authoritatively in
 // session.reply. We only keep the turn marked active. (This keeps the
 // final answer out of the status strip by construction.)
        s.proc.active = true;
        s.lastActivity = Date.now();
        break;
      }
      case "session.tool": {
        const s = this.route(env);
        if (!s) break;
 // A tool call is process → status strip.
        s.proc.active = true;
        s.proc.steps.push({ id: newId(), kind: "tool", text: `${env.body.name}(${env.body.params ?? ""})` });
        s.lastActivity = Date.now();
        break;
      }
      case "session.reply": {
        const s = this.route(env);
        if (!s) break;
 // The single point the answer enters the chat: the gateway sends the full
 // assembled assistant text here (the same text it persists), so we use it
 // verbatim — no client-side reconstruction. Then clear the status strip.
        const final = (env.body.text ?? "").trim();
        if (final) s.messages.push({ id: newId(), role: "assistant", text: final, ts: Date.now() / 1000 });
        s.awaiting = false;
        s.proc = emptyProc();   // clear the status strip
        s.lastActivity = Date.now();
        break;
      }
      case "session.user_message": {
 // The inbound user message — emitted by the daemon at the start of every
 // accepted turn so IM-originated sessions render the user side of the
 // conversation, not just the bot's reply. Console-originated turns ALSO
 // fire this event (the daemon doesn't distinguish), so dedupe by sessionKey:
 // the Composer's send() already optimistically pushed the message before
 // hitting the wire, and we'd render two copies otherwise. Mark the session
 // as awaiting so the typing indicator engages for IM turns the same way it
 // did for Console (Composer.send did it manually there).
        const key = env.body?.sessionKey ?? "";
        if (key === CONSOLE_UID || key === "console") break;
        const s = this.route(env);
        if (!s) break;
        const text = env.body?.text ?? "";
        const ts = env.body?.ts ? Number(env.body.ts) : Date.now() / 1000;
        s.messages.push({ id: newId(), role: "user", text, ts });
        s.awaiting = true;
        s.awaitingSince = Date.now();
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
 // Daemon error envelopes carry.message; older / odd shapes might
 // not. Fall back to an actionable string (with scope hint when
 // present) rather than the bare literal "error" — the prior code
 // surfaced the word "error" alone, which gives the user nothing.
        const scope = env.body?.scope ? `[${env.body.scope}] ` : "";
        this.setError(scope + (env.body?.message ?? "网关返回未知错误（无详情）"));
        break;
      }
      case "bridge.status": {
 // Synthetic event from the Go bridge: the control-bus connection state.
 // Lets the UI show "reconnecting" instead of silently freezing when the
 // daemon drops, and clear it when the bus comes back.
        const prev = this.connected;
        this.connected = !!env.body?.connected;
        if (this.connected) {
 // Daemon reachable again — drop any dismissal so a fresh failure
 // later can re-pin its banner.
          this.dismissedError = "";
 // On reconnect after a disconnect, cancel any sessions still
 // waiting on a turn that started before the drop — the killed
 // daemon won't emit turnDone for those, so Composer.canSend
 // would stay false (gated on !awaiting) until sweepStuckTurns
 // fires at TURN_MAX_MS (22 min). Operator can re-send instead
 // of waiting; the original message stays in transcript.
          if (!prev) {
            for (const s of this.sessions) {
              if (s.awaiting) {
                s.awaiting = false;
                s.proc = emptyProc();
              }
            }
          }
        }
        if (!this.connected && env.body?.detail) this.setError(env.body.detail);
        break;
      }
    }
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

 // ensureSession returns the existing Session or creates a placeholder. When
 // initialActivity is omitted, new sessions get lastActivity=Date.now so
 // a freshly-arriving turn floats to the top of the sidebar. Callers that
 // are NOT driven by a real activity event (e.g. CommandPalette jumping
 // into a stale session, loadHistory pre-creating a row for the response
 // to fold into) should pass 0 so the bogus Date.now doesn't permanently
 // out-sort genuinely-recent sessions — applyHistory + applySessionsList
 // both `Math.max(s.lastActivity, persisted)`, so a bogus high value
 // wins forever.
  private ensureSession(botId: string, key: string, initialActivity = Date.now()): Session {
    let s = this.sessions.find((x) => x.botId === botId && x.key === key);
    if (!s) {
      s = { botId, key, title: prettyTitle(key), messages: [], awaiting: false, awaitingSince: 0, proc: emptyProc(), inputTokens: 0, outputTokens: 0, cachedInputTokens: 0, costUsd: 0, lastActivity: initialActivity };
      this.sessions.push(s);
    }
 // Surface an arriving conversation when nothing is selected yet (first
 // connect, incoming IM, or an externally-driven turn) so the transcript
 // isn't left on the empty state while messages stream into the list.
    if (!this.selectedBotId) this.selectedBotId = botId;
    if (this.selectedKey == null && botId === this.selectedBotId) this.selectedKey = key;
    return s;
  }

  private applyHistory(botId: string, key: string, rows: any[]) {
 // Route by the botId+key the RESPONSE carries, not currentSession — the user
 // may have switched sessions while this fetch was in flight, and folding the
 // rows into whatever happens to be selected would cross-contaminate sessions
 // and permanently mark the wrong one loaded.
    const bid = botId || this.defaultBotId();
    if (!bid || !key) return;
    const s = this.sessions.find((x) => x.botId === bid && x.key === key);
    if (!s) return;
 // Drop responses whose loadHistory was issued before the most recent
 // reset() — the operator already cleared the visible transcript;
 // restoring it would be a surprise. expectedHistoryEpoch is unset for
 // the first fetch (treated as epoch 0).
    if ((s.expectedHistoryEpoch ?? 0) !== (s.historyEpoch ?? 0)) return;
    s.loaded = true; // mark fetched even when empty, so we don't refetch on every open
 // previously we silently dropped server history when
 // the user had typed before the lazy History response landed. Now
 // merge — keep any local-only messages on top of the persisted rows,
 // preserving order.
 // dedupe by (role, text, ts) tuple instead of a
 // strict ts > cutoff comparison. The persisted-row ts and the local
 // user-message ts collide at second granularity (both use
 // Date.now/1000), so a strict `>` was dropping the most recent
 // user message every time the boundary lined up. The tuple compare
 // matches when the server has acknowledged the message, otherwise
 // keeps the local copy.
    const persisted = rows.map((r) => ({
      id: newId(), role: r.role as Role, text: r.content, ts: r.ts,
    }));
    if (s.messages.length === 0) {
      s.messages = persisted;
      return;
    }
 // count by (role, text, floored-ts) tuple — a Set incorrectly dedupes
 // two distinct user messages with identical text that landed in the
 // same wall-clock second (e.g. an operator retry "ok"/"ok"), dropping
 // the second local copy in favor of the persisted row. Decrementing
 // the count means each persisted occurrence matches at most ONE local
 // copy; extra local duplicates stay visible.
    const counts = new Map<string, number>();
    for (const m of persisted) {
      const k = `${m.role}\x00${m.text}\x00${Math.floor(m.ts)}`;
      counts.set(k, (counts.get(k) ?? 0) + 1);
    }
    const localOnly = s.messages.filter((m) => {
      const k = `${m.role}\x00${m.text}\x00${Math.floor(m.ts)}`;
      const n = counts.get(k) ?? 0;
      if (n > 0) {
        counts.set(k, n - 1);
        return false;
      }
      return true;
    });
    s.messages = persisted.concat(localOnly);
  }

 // applySessionsList folds the persisted session roster (newest first) into the
 // store so the list survives restarts. Each summary carries a preview only; the
 // transcript is lazy-loaded on open (loadHistory). updatedAt is Unix seconds.
 // botId comes from the response so rows are never folded into the wrong bot when
 // the user switched bots mid-fetch.
  private applySessionsList(botId: string, rows: any[]) {
    const bid = botId || this.defaultBotId();
    if (!bid) return;
    for (const r of rows) {
      if (!r?.key) continue;
      const existed = this.sessions.some((x) => x.botId === bid && x.key === r.key);
      const s = this.ensureSession(bid, r.key);
 // Don't clobber a live session's recency: a turn in flight sets lastActivity
 // to Date.now; the persisted updatedAt is older, so only seed it for
 // sessions we just created (or take the max), or an active chat would drop
 // down the list mid-turn.
      const persisted = (r.updatedAt ?? 0) * 1000;
      s.lastActivity = existed ? Math.max(s.lastActivity, persisted) : persisted;
      s.preview = r.preview ?? "";
    }
 // First roster after connect: if nothing is selected yet, open the newest
 // (or fall back to Console — see initialKey).
    if (this.selectedKey == null) {
      this.selectedKey = this.initialKey();
    }
    if (this.selectedKey) this.loadHistory(this.selectedKey);
  }

 // sweepStuckTurns clears awaiting on any session whose turnStart is older
 // than TURN_MAX_MS without a terminal event. Surfaces a synthetic error
 // so the user knows the turn didn't just hang silently.
  private sweepStuckTurns() {
    const now = Date.now();
    for (const s of this.sessions) {
      if (s.awaiting && s.awaitingSince > 0 && now - s.awaitingSince > TURN_MAX_MS) {
        s.awaiting = false;
        s.awaitingSince = 0;
        s.proc = emptyProc();
        this.setError("[turn] 长时间未收到响应，已取消等待 — 请重试或检查网关");
      }
    }
  }
}

export const store = new Store();

// Vite HMR: clear the prior module's interval before the new module instantiates
// its own. Strips the warning otherwise rendered by Svelte 5 about reactive
// state spread across stale module copies.
if (import.meta.hot) {
  import.meta.hot.dispose(() => store.dispose());
}
