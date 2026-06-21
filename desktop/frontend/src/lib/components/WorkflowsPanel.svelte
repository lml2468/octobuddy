<script lang="ts">
  import { XClawService } from "../../../bindings/github.com/lml2468/xclaw/desktop";
  import { store } from "../store.svelte";
  import { modal } from "../actions/modal";
  import SettingsHeader from "./SettingsHeader.svelte";
  import Confirm from "./Confirm.svelte";

  let { onclose, onedit, onskills, onusage }: { onclose: () => void; onedit?: () => void; onskills?: () => void; onusage?: () => void } = $props();

  type WfInfo = { name: string; description: string };
  const isPreview = new URLSearchParams(location.search).has("preview");

  let botId = $state<string | null>(store.selectedBotId ?? store.bots[0]?.id ?? null);

  // Adopt the first bot once the roster loads, if the panel opened before it did.
  $effect(() => {
    if (botId == null && store.bots.length) {
      botId = store.selectedBotId ?? store.bots[0].id;
      if (isPreview) loadBotPreview(); else loadBot();
    }
  });

  let botWfs = $state<WfInfo[]>([]);
  let sel = $state<string | null>(null);
  let content = $state("");
  let dirty = $state(false);
  let error = $state("");
  let newName = $state("");

  // window.confirm() is a no-op in the Wails webview — use the shared <Confirm>.
  type ConfirmReq = { msg: string; danger?: boolean; confirmLabel?: string; resolve: (v: boolean) => void };
  let confirmState = $state<ConfirmReq | null>(null);
  function ask(msg: string, opts?: { danger?: boolean; confirmLabel?: string }): Promise<boolean> {
    return new Promise((resolve) => (confirmState = { msg, resolve, danger: opts?.danger, confirmLabel: opts?.confirmLabel }));
  }
  function answer(v: boolean) { confirmState?.resolve(v); confirmState = null; }
  async function leave(fn?: () => void) {
    if (dirty && !(await ask("有未保存的改动,确认离开?", { confirmLabel: "离开" }))) return;
    onclose(); fn?.();
  }

  // Preview-mode in-memory state: bot id → { name → script source }.
  const mockBot: Record<string, Record<string, string>> = {
    main: {
      "review-changes": "export const meta = {\n  name: 'review-changes',\n  description: 'Review the diff across dimensions and verify each finding.',\n  phases: [{ title: 'Review' }, { title: 'Verify' }],\n}\nphase('Review')\nreturn { ok: true }\n",
    },
    research: {},
  };

  load();

  async function load() {
    error = "";
    try {
      if (isPreview) { loadBotPreview(); return; }
      await loadBot();
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  function loadBotPreview() {
    const b = botId ?? "";
    botWfs = Object.entries(mockBot[b] ?? {}).map(([name, src]) => ({ name, description: descOf(src) }));
    if (botWfs.length && !botWfs.find((w) => w.name === sel)) select(botWfs[0].name);
    else if (!botWfs.length) { sel = null; content = ""; }
  }

  async function loadBot() {
    if (!botId) { botWfs = []; sel = null; return; }
    botWfs = ((await XClawService.BotWorkflowsList(botId)) ?? []) as WfInfo[];
    if (botWfs.length && !botWfs.find((w) => w.name === sel)) select(botWfs[0].name);
    else if (!botWfs.length) { sel = null; content = ""; }
  }

  async function switchBot(id: string) {
    if (dirty && !(await ask("放弃未保存的改动?", { confirmLabel: "放弃", danger: true }))) return;
    botId = id; sel = null; content = ""; dirty = false;
    if (isPreview) loadBotPreview(); else await loadBot();
  }

  function descOf(src: string): string {
    const m = src.match(/description\s*:\s*["']([^"']+)["']/);
    return m ? m[1] : "";
  }

  async function select(name: string) {
    if (dirty && !(await ask("放弃未保存的改动?", { confirmLabel: "放弃", danger: true }))) return;
    sel = name; error = "";
    try {
      content = isPreview ? (mockBot[botId ?? ""]?.[name] ?? "") : await XClawService.BotWorkflowRead(botId!, name);
      dirty = false;
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function save() {
    if (!sel) return;
    try {
      if (isPreview) { mockBot[botId ?? ""][sel] = content; }
      else await XClawService.BotWorkflowWrite(botId!, sel, content);
      dirty = false;
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function createOwn() {
    const name = newName.trim();
    if (!name || !botId) return;
    try {
      if (isPreview) { (mockBot[botId] ??= {})[name] = `export const meta = {\n  name: '${name}',\n  description: 'One line on what this workflow does.',\n}\nreturn { ok: true }\n`; loadBotPreview(); }
      else { await XClawService.BotWorkflowCreate(botId, name); await loadBot(); }
      newName = "";
      select(name);
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function removeBotWf(w: WfInfo) {
    if (!(await ask(`删除「${w.name}」?`, { confirmLabel: "删除", danger: true }))) return;
    try {
      if (isPreview) { delete mockBot[botId ?? ""][w.name]; loadBotPreview(); }
      else { await XClawService.BotWorkflowDelete(botId!, w.name); await loadBot(); }
      if (sel === w.name) { sel = null; content = ""; dirty = false; }
    } catch (e: any) { error = String(e?.message ?? e); }
  }
</script>

<div class="scrim" onclick={() => leave()} role="presentation">
  <!-- svelte-ignore a11y_click_events_have_key_events (use:modal handles Escape/Tab; this onclick only stops propagation) -->
  <div class="modal" use:modal={{ onclose: () => leave() }} onclick={(e) => e.stopPropagation()} role="dialog" aria-label="工作流" tabindex="-1">
    <SettingsHeader active="workflows" onclose={() => leave()} onnav={leave} {onedit} {onskills} {onusage}>
      <label class="botpick">
        <span>Bot</span>
        <select value={botId} onchange={(e) => switchBot((e.currentTarget as HTMLSelectElement).value)}>
          {#each (isPreview ? [{ id: "main" }, { id: "research" }] : store.bots) as b (b.id)}
            <option value={b.id}>{b.id}</option>
          {/each}
        </select>
      </label>
    </SettingsHeader>

    <div class="body">
      <div class="list">
        {#if !botId}
          <div class="muted">先选择一个 Bot</div>
        {:else}
          <div class="sectlbl">本 Bot 工作流</div>
          {#each botWfs as w (w.name)}
            <div class="row" class:sel={w.name === sel}>
              <button class="rowmain" onclick={() => select(w.name)}>
                <span class="nm">{w.name}</span>
                <span class="ds">{w.description || "暂无描述"}</span>
              </button>
              <button class="del" title="删除" onclick={() => removeBotWf(w)}>−</button>
            </div>
          {/each}
          {#if botWfs.length === 0}<div class="muted">该 Bot 还没有工作流</div>{/if}
          <div class="new">
            <input placeholder="新建工作流名称" bind:value={newName} onkeydown={(e) => e.key === "Enter" && createOwn()} />
            <button class="add" onclick={createOwn} disabled={!newName.trim()}>+ 新建工作流</button>
          </div>
        {/if}
      </div>

      {#if sel}
        <div class="editor">
          <div class="ehead">
            <span class="dt">{sel}.js</span>
            <span class="spacer"></span>
            {#if dirty}<span class="dirty">●</span>{/if}
            <button class="primary" onclick={save} disabled={!dirty}>保存</button>
          </div>
          <textarea class="code" bind:value={content} oninput={() => (dirty = true)} spellcheck="false"></textarea>
        </div>
      {:else}
        <div class="editor"><div class="muted center">选择或新建一个工作流</div></div>
      {/if}
    </div>

    {#if error}
      <footer class="errbar"><span class="err">⚠️ {error}</span><button class="dismiss" onclick={() => (error = "")} aria-label="清除">✕</button></footer>
    {/if}

    {#if confirmState}
      <Confirm
        message={confirmState.msg}
        confirmLabel={confirmState.confirmLabel ?? "确认"}
        danger={confirmState.danger ?? false}
        onresult={answer}
      />
    {/if}
  </div>
</div>

<style>
  /* Mirrors ConfigEditor / SkillsPanel: full-window scrim + glass + shared SettingsHeader. */
  .scrim { position: fixed; inset: 0; z-index: 50; background: var(--window-grad); display: block; }
  .modal { width: 100%; height: 100%; position: relative; display: flex; flex-direction: column; background: var(--glass); backdrop-filter: blur(24px) saturate(180%); -webkit-backdrop-filter: blur(24px) saturate(180%); border: none; border-radius: 0; box-shadow: none; overflow: hidden; color: var(--ink); font-family: var(--ui); }
  .row:focus-visible, .rowmain:focus-visible, .add:focus-visible, .primary:focus-visible, .del:focus-visible { outline: none; box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 30%, transparent); }

  .botpick { display: inline-flex; align-items: center; gap: 7px; font-size: 12px; color: var(--ink-soft); -webkit-app-region: no-drag; }
  .botpick select { background: color-mix(in srgb, var(--ink) 5%, var(--surface)); border: 1px solid var(--hairline); border-radius: 8px; padding: 5px 9px; color: var(--ink); font-size: 12px; font-family: var(--mono); outline: none; }

  .body { flex: 1; display: grid; grid-template-columns: 280px 1fr; overflow: hidden; }
  .list { border-right: 1px solid var(--hairline); padding: 10px; display: flex; flex-direction: column; gap: 3px; overflow-y: auto; background: color-mix(in srgb, var(--ink) 3%, transparent); }
  .sectlbl { font-size: 11px; font-weight: 600; color: var(--ink-faint); text-transform: uppercase; letter-spacing: .04em; padding: 6px 6px 3px; }
  .row { display: flex; align-items: center; border-radius: 8px; }
  .row:hover { background: color-mix(in srgb, var(--ink) 5%, transparent); }
  .row.sel { background: color-mix(in srgb, var(--accent) 16%, transparent); }
  .rowmain { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; text-align: left; padding: 8px 10px; border: none; background: transparent; color: var(--ink); }
  .nm { font-size: 13px; font-weight: 600; font-family: var(--mono); display: flex; align-items: center; gap: 6px; }
  .ds { font-size: 11px; color: var(--ink-soft); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .del { width: 24px; height: 24px; flex: 0 0 auto; border: none; background: transparent; color: var(--ink-faint); font-size: 15px; }
  .del:hover { color: var(--danger); }
  .muted { color: var(--ink-faint); font-size: 12px; padding: 12px; }
  .muted.center { display: grid; place-items: center; height: 100%; }

  .new { display: flex; flex-direction: column; gap: 6px; margin-top: 6px; }
  input { background: color-mix(in srgb, var(--ink) 4%, var(--surface)); border: 1px solid var(--hairline); border-radius: var(--radius-control); padding: 8px 11px; color: var(--ink); font-size: 12px; font-family: var(--mono); outline: none; transition: border-color .15s ease, box-shadow .15s ease; }
  input:focus { border-color: color-mix(in srgb, var(--accent) 55%, var(--hairline)); box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 16%, transparent); }
  .add { text-align: center; padding: 7px 10px; border: 1px dashed var(--hairline); background: transparent; border-radius: 8px; color: var(--ink-soft); font-size: 12px; transition: border-color .14s ease, color .14s ease; }
  .add:hover:not(:disabled) { border-color: color-mix(in srgb, var(--accent) 45%, var(--hairline)); color: var(--accent-strong); }
  .add:disabled { opacity: 0.45; }

  .editor { display: flex; flex-direction: column; min-width: 0; }
  .ehead { display: flex; align-items: center; gap: 8px; padding: 10px 14px; border-bottom: 1px solid var(--hairline); }
  .dt { font-size: 13px; font-weight: 600; font-family: var(--mono); }
  .spacer { flex: 1; }
  .dirty { color: var(--accent); font-size: 10px; }
  .primary { background: linear-gradient(135deg, var(--grad-a), var(--grad-b)); color: #fff; border: none; border-radius: 9px; padding: 7px 15px; font-size: 12px; font-weight: 550; box-shadow: 0 3px 12px color-mix(in srgb, var(--grad-a) 40%, transparent); transition: transform .12s ease, box-shadow .14s ease, opacity .14s ease; }
  .primary:hover:not(:disabled) { transform: translateY(-1px); box-shadow: 0 6px 18px color-mix(in srgb, var(--grad-a) 50%, transparent); }
  .primary:disabled { opacity: 0.45; }
  textarea.code { flex: 1; resize: none; border: none; outline: none; background: var(--code-bg); color: var(--ink); padding: 12px 14px; font-family: var(--mono); font-size: 12.5px; line-height: 1.6; }

  .err { color: var(--danger); font-size: 12px; }
  .errbar { display: flex; align-items: center; gap: 8px; padding: 8px 18px; border-top: 1px solid var(--hairline); background: color-mix(in srgb, var(--danger) 6%, var(--surface)); }
  .errbar .err { flex: 1; }
  .errbar .dismiss { width: 24px; height: 24px; display: grid; place-items: center; background: none; border: none; border-radius: 6px; color: var(--ink-soft); font-size: 11px; }
  .errbar .dismiss:hover { background: color-mix(in srgb, var(--ink) 8%, transparent); color: var(--ink); }
  .errbar .dismiss:focus-visible { outline: none; box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 30%, transparent); }
</style>
