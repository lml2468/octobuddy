<script lang="ts">
  import { XClawService } from "../../../bindings/github.com/lml2468/xclaw/desktop";
  import { BotConfig } from "../../../bindings/github.com/lml2468/xclaw/desktop/internal/configstore/models";
  import { store } from "../store.svelte";
  import Avatar from "./Avatar.svelte";
  import Confirm from "./Confirm.svelte";
  import SettingsHeader from "./SettingsHeader.svelte";
  import { modal } from "../actions/modal";

  let { onclose, onskills, onusage, onworkflows }: { onclose: () => void; onskills?: () => void; onusage?: () => void; onworkflows?: () => void } = $props();

  let bots = $state<BotConfig[]>([]);
  let sel = $state(0);
  let error = $state("");
  let saved = $state(false);
  let busy = $state(false);
  let dirty = $state(false);
  // Pending navigation held behind the unsaved-changes confirm.
  let pendingLeave = $state<null | (() => void)>(null);

  // Guarded navigation/close: if the form has unsaved edits, ask first.
  function leave(fn?: () => void) {
    const go = () => { onclose(); fn?.(); };
    if (dirty) { pendingLeave = go; return; }
    go();
  }
  function resolveLeave(ok: boolean) {
    const go = pendingLeave;
    pendingLeave = null;
    if (ok && go) go();
  }

  const current = $derived(bots[sel] ?? null);
  // Env as an editable list of pairs (kept in sync with current.env on edit).
  let envRows = $state<{ k: string; v: string }[]>([]);
  // Bot ids present when this editor opened — the basis for an EXPLICIT removal
  // list on save (so the daemon never infers deletions from a set-difference).
  let loadedIds: string[] = [];

  $effect(() => {
    // Rebuild env rows when the selected bot changes.
    sel;
    const e = bots[sel]?.env ?? {};
    envRows = Object.entries(e).map(([k, v]) => ({ k, v: String(v ?? "") }));
  });

  load();

  async function load() {
    if (new URLSearchParams(location.search).has("preview")) {
      bots = [
        new BotConfig({ id: "main", apiUrl: "https://im.example.com/api", model: "claude-opus-4-8", gatewayBaseUrl: "https://gw.example/v1", env: { OCTO_BOT_ID: "main-7f3a" }, soul: "You are Atlas, the team's ops copilot.", agents: "Confirm before destructive actions." }),
        new BotConfig({ id: "research", apiUrl: "https://im.example.com/api" }),
      ];
      sel = 0;
      return;
    }
    try {
      bots = (await XClawService.LoadConfig()) ?? [];
      loadedIds = bots.map((b) => b.id);
      if (bots.length === 0) addBot();
      sel = 0;
    } catch (e: any) {
      error = String(e);
    }
  }

  function addBot() {
    let n = 1;
    const ids = new Set(bots.map((b) => b.id));
    while (ids.has(`bot${n}`)) n++;
    const b = new BotConfig({ id: `bot${n}`, apiUrl: bots[0]?.apiUrl ?? "https://" });
    bots = [...bots, b];
    sel = bots.length - 1;
  }

  function removeBot(i: number) {
    bots = bots.filter((_, idx) => idx !== i);
    sel = Math.max(0, Math.min(sel, bots.length - 1));
  }

  function commitEnv() {
    if (!current) return;
    const m: { [k: string]: string } = {};
    for (const r of envRows) if (r.k.trim()) m[r.k.trim()] = r.v;
    current.env = m;
  }

  async function save(restart: boolean) {
    if (!current) return;
    commitEnv();
    error = ""; saved = false; busy = true;
    try {
      // Explicit removals: ids that were loaded but are no longer present.
      const present = new Set(bots.map((b) => b.id));
      const removed = loadedIds.filter((id) => !present.has(id));
      await XClawService.SaveConfig(bots, removed);
      loadedIds = bots.map((b) => b.id);
      saved = true;
      dirty = false;
      if (restart) { await XClawService.RestartCore(); store.bots = []; XClawService.BotsList(); onclose(); }
    } catch (e: any) {
      error = String(e?.message ?? e);
    } finally {
      busy = false;
    }
  }
</script>

<div class="scrim" onclick={() => leave()} role="presentation">
  <!-- svelte-ignore a11y_click_events_have_key_events (use:modal handles Escape/Tab; this onclick only stops propagation) -->
  <div class="modal" use:modal={{ onclose: () => leave() }} onclick={(e) => e.stopPropagation()} role="dialog" aria-label="编辑 Bot" tabindex="-1">
    <SettingsHeader active="editor" onclose={() => leave()} onnav={leave} {onskills} {onusage} {onworkflows} />

    <div class="body">
      <div class="bots">
        {#each bots as b, i (i)}
          <button class="botrow" class:sel={i === sel} onclick={() => (sel = i)}>
            <Avatar name={b.id || "bot"} size={26} />
            <span class="bn">{b.id || "(未命名)"}</span>
            <span class="bdot" class:on={store.bots.find((x) => x.id === b.id)?.connected}></span>
          </button>
        {/each}
        <button class="add" onclick={addBot}>+ 新增 Bot</button>
      </div>

      {#if current}
        <div class="form" oninput={() => (dirty = true)} onchange={() => (dirty = true)}>
          <div class="grid2">
            <label>Bot ID <input bind:value={current.id} placeholder="my-bot" /></label>
            <label>模型 <input bind:value={current.model} placeholder="claude-opus-4-8" /></label>
          </div>
          <label>API URL <input bind:value={current.apiUrl} placeholder="https://octo-server" /></label>
          <div class="grid2">
            <label>Bot Token
              <input type="password" bind:value={current.octoToken} placeholder="bf_…" />
              <small>存于系统钥匙串,绝不写入 config.json。</small>
            </label>
            <label>网关地址(可选)<input bind:value={current.gatewayBaseUrl} placeholder="https://gateway/v1" /></label>
          </div>
          <label>网关 Token(可选)<input type="password" bind:value={current.gatewayToken} placeholder="sk-…" /></label>

          <div class="env">
            <span class="lbl">环境变量</span>
            {#each envRows as row, i (i)}
              <div class="envrow">
                <input class="k" bind:value={row.k} placeholder="KEY" />
                <span>=</span>
                <input class="v" bind:value={row.v} placeholder="value" />
                <button class="del" onclick={() => (envRows = envRows.filter((_, x) => x !== i))} aria-label="删除">−</button>
              </div>
            {/each}
            <button class="add sm" onclick={() => (envRows = [...envRows, { k: "", v: "" }])}>+ 添加变量</button>
          </div>

          <div class="skills">
            <span class="lbl">技能 / 工作流</span>
            <small>在「技能」「工作流」分页里为本 Bot 安装市场内容或维护自有内容。</small>
          </div>

          <label>SOUL.md <textarea bind:value={current.soul} rows="3" placeholder="身份、语气、角色"></textarea></label>
          <label>AGENTS.md <textarea bind:value={current.agents} rows="3" placeholder="规范、可做与不可做"></textarea></label>

          <button class="remove" onclick={() => removeBot(sel)}>删除此 Bot</button>
        </div>
      {/if}
    </div>

    <footer>
      {#if error}<span class="err">⚠️ {error}</span>{:else if saved}<span class="ok">✓ 已保存</span>{/if}
      <span class="spacer"></span>
      <button onclick={() => save(false)} disabled={busy}>保存</button>
      <button class="primary" onclick={() => save(true)} disabled={busy}>保存并重启</button>
    </footer>

    {#if pendingLeave}
      <Confirm message="有未保存的改动,确认离开?" confirmLabel="离开" onresult={resolveLeave} />
    {/if}
  </div>
</div>

<style>
  .scrim { position: fixed; inset: 0; z-index: 50; background: var(--window-grad); display: block; }
  .modal { width: 100%; height: 100%; display: flex; flex-direction: column; background: var(--glass); backdrop-filter: blur(24px) saturate(180%); -webkit-backdrop-filter: blur(24px) saturate(180%); border: none; border-radius: 0; box-shadow: none; overflow: hidden; }

  .body { flex: 1; display: grid; grid-template-columns: 210px 1fr; overflow: hidden; }
  .bots { border-right: 1px solid var(--border-soft, var(--hairline)); padding: 10px; display: flex; flex-direction: column; gap: 3px; overflow-y: auto; }
  .botrow { display: flex; align-items: center; gap: 9px; text-align: left; padding: 7px 9px; border: none; background: transparent; border-radius: 9px; color: var(--ink-soft); }
  .botrow .bn { font-size: 13px; font-weight: 550; flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .bdot { width: 8px; height: 8px; flex: 0 0 auto; border-radius: 50%; background: var(--muted); }
  .bdot.on { background: var(--online, var(--success)); }
  .botrow:hover { background: color-mix(in srgb, var(--ink) 5%, transparent); }
  .botrow.sel { background: color-mix(in srgb, var(--accent) 14%, transparent); color: var(--ink); }
  .add { text-align: center; padding: 8px 10px; border: 1px dashed var(--hairline); background: transparent; border-radius: 9px; color: var(--ink-soft); margin-top: 4px; }
  .add.sm { font-size: 12px; padding: 5px 8px; text-align: left; }

  .form { padding: 18px 20px; overflow-y: auto; display: flex; flex-direction: column; gap: 14px; }
  .grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
  label { display: flex; flex-direction: column; gap: 5px; font-size: 12px; font-weight: 550; letter-spacing: 0.01em; color: var(--ink-soft); }
  input, textarea { background: color-mix(in srgb, var(--ink) 4%, var(--surface)); border: 1px solid var(--hairline); border-radius: 10px; padding: 8px 11px; color: var(--ink); font-size: 13px; outline: none; transition: border-color .15s ease, box-shadow .15s ease; }
  input:focus, textarea:focus { border-color: color-mix(in srgb, var(--accent) 55%, var(--hairline)); box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 16%, transparent); }
  textarea { resize: vertical; font-family: var(--ui); }
  small { color: var(--ink-faint); font-size: 11px; }

  .env { display: flex; flex-direction: column; gap: 6px; }
  .lbl { font-size: 12px; color: var(--ink-soft); }
  .skills { display: flex; flex-direction: column; gap: 5px; }
  .skills small { color: var(--ink-faint); font-size: 11px; }
  .envrow { display: flex; align-items: center; gap: 6px; }
  .envrow .k { width: 160px; font-family: var(--mono); font-size: 12px; }
  .envrow .v { flex: 1; }
  .del { width: 26px; height: 26px; border-radius: 8px; border: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 4%, var(--surface)); color: var(--ink-soft); }
  .remove { align-self: flex-start; margin-top: 6px; color: var(--danger); background: transparent; border: 1px solid color-mix(in srgb, var(--danger) 40%, var(--hairline)); border-radius: 4px; padding: 6px 12px; }

  footer { display: flex; align-items: center; gap: 10px; padding: 12px 18px; border-top: 1px solid var(--hairline); }
  .spacer { flex: 1; }
  footer button { padding: 8px 18px; border-radius: 10px; border: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 4%, var(--surface)); color: var(--ink); font-weight: 550; transition: background .14s ease, transform .12s ease, box-shadow .14s ease; }
  footer button:hover { background: color-mix(in srgb, var(--ink) 8%, var(--surface)); }
  footer button:active { transform: translateY(1px); }
  footer button:disabled { opacity: .5; cursor: default; transform: none; box-shadow: none; }
  footer .primary { background: linear-gradient(135deg, var(--grad-a), var(--grad-b)); color: #fff; border: none; box-shadow: 0 4px 14px color-mix(in srgb, var(--grad-a) 45%, transparent); }
  footer .primary:hover { transform: translateY(-1px); box-shadow: 0 8px 22px color-mix(in srgb, var(--grad-a) 52%, transparent); }

  /* interaction states + keyboard focus (WCAG 2.4.7) */
  .botrow:focus-visible, footer button:focus-visible, .add:focus-visible, .remove:focus-visible { outline: none; box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 30%, transparent); }
  .add:hover { border-color: color-mix(in srgb, var(--accent) 45%, var(--hairline)); color: var(--accent-strong, var(--accent)); }
  .remove:hover { background: color-mix(in srgb, var(--danger) 10%, transparent); }
  .err { color: var(--danger); font-size: 12px; }
  .ok { color: #5aa873; font-size: 12px; }
</style>
