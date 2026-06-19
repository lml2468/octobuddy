<script lang="ts">
  import { XClawService } from "../../../bindings/github.com/lml2468/xclaw/desktop";
  import { store } from "../store.svelte";
  import { modal } from "../actions/modal";
  import SettingsHeader from "./SettingsHeader.svelte";

  let { onclose, onedit, onskills, onusage }: { onclose: () => void; onedit?: () => void; onskills?: () => void; onusage?: () => void } = $props();

  type WfInfo = { name: string; description: string; installed: boolean };
  const isPreview = new URLSearchParams(location.search).has("preview");

  let botId = $state<string | null>(store.selectedBotId ?? store.bots[0]?.id ?? null);

  let botWfs = $state<WfInfo[]>([]); // this bot's own + installed workflows
  let catalog = $state<WfInfo[]>([]); // global marketplace catalog
  let sel = $state<string | null>(null);
  let content = $state("");
  let dirty = $state(false);
  let error = $state("");
  let newName = $state("");

  const selInstalled = $derived(botWfs.find((w) => w.name === sel)?.installed ?? false);
  const installedNames = $derived(new Set(botWfs.filter((w) => w.installed).map((w) => w.name)));

  // window.confirm() is a no-op in the Wails webview — use an in-app dialog.
  let confirmState = $state<{ msg: string; resolve: (v: boolean) => void } | null>(null);
  function ask(msg: string): Promise<boolean> {
    return new Promise((resolve) => (confirmState = { msg, resolve }));
  }
  function answer(v: boolean) { confirmState?.resolve(v); confirmState = null; }
  async function leave(fn?: () => void) {
    if (dirty && !(await ask("有未保存的改动,确认离开?"))) return;
    onclose(); fn?.();
  }

  const mockCatalog: Record<string, string> = {
    "review-changes": "export const meta = {\n  name: 'review-changes',\n  description: 'Review the diff across dimensions and verify each finding.',\n  phases: [{ title: 'Review' }, { title: 'Verify' }],\n}\nphase('Review')\nreturn { ok: true }\n",
    "deep-audit": "export const meta = {\n  name: 'deep-audit',\n  description: 'Exhaustive multi-pass audit with adversarial verification.',\n}\nreturn { ok: true }\n",
  };
  const mockBot: Record<string, Record<string, { installed: boolean; src: string }>> = {
    main: { "review-changes": { installed: true, src: mockCatalog["review-changes"] } },
    research: {},
  };

  load();

  async function load() {
    error = "";
    try {
      if (isPreview) {
        catalog = Object.entries(mockCatalog).map(([name, src]) => ({ name, description: descOf(src), installed: false }));
        loadBotPreview();
        return;
      }
      catalog = ((await XClawService.WorkflowsList()) ?? []) as WfInfo[];
      await loadBot();
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  function loadBotPreview() {
    const b = botId ?? "";
    botWfs = Object.entries(mockBot[b] ?? {}).map(([name, w]) => ({ name, description: descOf(w.src), installed: w.installed }));
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
    if (dirty && !(await ask("放弃未保存的改动?"))) return;
    botId = id; sel = null; content = ""; dirty = false;
    if (isPreview) loadBotPreview(); else await loadBot();
  }

  function descOf(src: string): string {
    const m = src.match(/description\s*:\s*["']([^"']+)["']/);
    return m ? m[1] : "";
  }

  async function select(name: string) {
    if (dirty && !(await ask("放弃未保存的改动?"))) return;
    sel = name; error = "";
    try {
      content = isPreview ? (mockBot[botId ?? ""]?.[name]?.src ?? "") : await XClawService.BotWorkflowRead(botId!, name);
      dirty = false;
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function save() {
    if (!sel || selInstalled) return;
    try {
      if (isPreview) { mockBot[botId ?? ""][sel].src = content; }
      else await XClawService.BotWorkflowWrite(botId!, sel, content);
      dirty = false;
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function createOwn() {
    const name = newName.trim();
    if (!name || !botId) return;
    try {
      if (isPreview) { (mockBot[botId] ??= {})[name] = { installed: false, src: `export const meta = {\n  name: '${name}',\n  description: 'One line on what this workflow does.',\n}\nreturn { ok: true }\n` }; loadBotPreview(); }
      else { await XClawService.BotWorkflowCreate(botId, name); await loadBot(); }
      newName = "";
      select(name);
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function removeBotWf(w: WfInfo) {
    const verb = w.installed ? "卸载" : "删除";
    if (!(await ask(`${verb}「${w.name}」?`))) return;
    try {
      if (isPreview) { delete mockBot[botId ?? ""][w.name]; loadBotPreview(); }
      else {
        if (w.installed) await XClawService.BotWorkflowUninstall(botId!, w.name);
        else await XClawService.BotWorkflowDelete(botId!, w.name);
        await loadBot();
      }
      if (sel === w.name) { sel = null; content = ""; dirty = false; }
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function install(name: string) {
    if (!botId) return;
    try {
      if (isPreview) { (mockBot[botId] ??= {})[name] = { installed: true, src: mockCatalog[name] }; loadBotPreview(); }
      else { await XClawService.BotWorkflowInstall(botId, name); await loadBot(); }
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function uninstall(name: string) {
    if (!botId) return;
    try {
      if (isPreview) { delete mockBot[botId ?? ""][name]; loadBotPreview(); }
      else { await XClawService.BotWorkflowUninstall(botId, name); await loadBot(); }
      if (sel === name) { sel = null; content = ""; dirty = false; }
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
                <span class="nm">{w.name}{#if w.installed}<span class="badge">已安装</span>{:else}<span class="badge own">自有</span>{/if}</span>
                <span class="ds">{w.description || "暂无描述"}</span>
              </button>
              <button class="del" title={w.installed ? "卸载" : "删除"} onclick={() => removeBotWf(w)}>−</button>
            </div>
          {/each}
          {#if botWfs.length === 0}<div class="muted">该 Bot 还没有工作流</div>{/if}
          <div class="new">
            <input placeholder="新建自有工作流名称" bind:value={newName} onkeydown={(e) => e.key === "Enter" && createOwn()} />
            <button class="add" onclick={createOwn} disabled={!newName.trim()}>+ 新建自有工作流</button>
          </div>

          <div class="sectlbl market">工作流市场</div>
          {#each catalog as c (c.name)}
            <div class="mrow">
              <span class="mnm">{c.name}</span>
              <span class="mds">{c.description || "暂无描述"}</span>
              {#if installedNames.has(c.name)}
                <button class="inst on" onclick={() => uninstall(c.name)}>已安装</button>
              {:else}
                <button class="inst" onclick={() => install(c.name)}>安装</button>
              {/if}
            </div>
          {/each}
          {#if catalog.length === 0}<div class="muted">市场为空</div>{/if}
        {/if}
      </div>

      {#if sel}
        <div class="editor">
          <div class="ehead">
            <span class="dt">{sel}.js</span>
            {#if selInstalled}<span class="robadge">市场工作流 · 只读</span>{/if}
            <span class="spacer"></span>
            {#if dirty}<span class="dirty">●</span>{/if}
            {#if !selInstalled}<button class="primary" onclick={save} disabled={!dirty}>保存</button>{/if}
          </div>
          <textarea class="code" bind:value={content} oninput={() => (dirty = true)} readonly={selInstalled} spellcheck="false"></textarea>
        </div>
      {:else}
        <div class="editor"><div class="muted center">选择一个工作流,或从市场安装</div></div>
      {/if}
    </div>

    {#if error}<div class="err">⚠️ {error}</div>{/if}

    {#if confirmState}
      <div class="confirm-scrim" role="presentation">
        <div class="confirm" role="alertdialog" aria-label="确认" tabindex="-1">
          <p>{confirmState.msg}</p>
          <div class="cbtns">
            <button onclick={() => answer(false)}>取消</button>
            <button class="danger" onclick={() => answer(true)}>确认</button>
          </div>
        </div>
      </div>
    {/if}
  </div>
</div>

<style>
  /* Mirrors ConfigEditor / SkillsPanel: full-window scrim + glass + shared SettingsHeader. */
  .scrim { position: fixed; inset: 0; z-index: 50; background: var(--window-grad); display: block; }
  .modal { width: 100%; height: 100%; position: relative; display: flex; flex-direction: column; background: var(--glass); backdrop-filter: blur(24px) saturate(180%); -webkit-backdrop-filter: blur(24px) saturate(180%); border: none; border-radius: 0; box-shadow: none; overflow: hidden; color: var(--ink); font-family: var(--ui); }
  .row:focus-visible, .rowmain:focus-visible, .add:focus-visible, .primary:focus-visible, .inst:focus-visible, .del:focus-visible, .cbtns button:focus-visible { outline: none; box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 30%, transparent); }

  .botpick { display: inline-flex; align-items: center; gap: 7px; font-size: 12px; color: var(--ink-soft); -webkit-app-region: no-drag; }
  .botpick select { background: color-mix(in srgb, var(--ink) 5%, var(--surface)); border: 1px solid var(--hairline); border-radius: 8px; padding: 5px 9px; color: var(--ink); font-size: 12px; font-family: var(--mono); outline: none; }

  .body { flex: 1; display: grid; grid-template-columns: 280px 1fr; overflow: hidden; }
  .list { border-right: 1px solid var(--hairline); padding: 10px; display: flex; flex-direction: column; gap: 3px; overflow-y: auto; background: color-mix(in srgb, var(--ink) 3%, transparent); }
  .sectlbl { font-size: 11px; font-weight: 600; color: var(--ink-faint); text-transform: uppercase; letter-spacing: .04em; padding: 6px 6px 3px; }
  .sectlbl.market { margin-top: 12px; border-top: 1px solid var(--hairline); padding-top: 12px; }
  .row { display: flex; align-items: center; border-radius: 8px; }
  .row:hover { background: color-mix(in srgb, var(--ink) 5%, transparent); }
  .row.sel { background: color-mix(in srgb, var(--accent) 16%, transparent); }
  .rowmain { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; text-align: left; padding: 8px 10px; border: none; background: transparent; color: var(--ink); }
  .nm { font-size: 13px; font-weight: 600; font-family: var(--mono); display: flex; align-items: center; gap: 6px; }
  .badge { font-size: 10px; font-weight: 600; padding: 1px 6px; border-radius: 6px; background: color-mix(in srgb, var(--accent) 18%, transparent); color: var(--accent-strong, var(--accent)); font-family: var(--ui); }
  .badge.own { background: color-mix(in srgb, var(--ink) 10%, transparent); color: var(--ink-soft); }
  .ds { font-size: 11px; color: var(--ink-soft); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .del { width: 24px; height: 24px; flex: 0 0 auto; border: none; background: transparent; color: var(--ink-faint); font-size: 15px; }
  .del:hover { color: var(--danger); }
  .muted { color: var(--ink-faint); font-size: 12px; padding: 12px; }
  .muted.center { display: grid; place-items: center; height: 100%; }

  .mrow { display: grid; grid-template-columns: 1fr auto; grid-template-rows: auto auto; column-gap: 8px; align-items: center; padding: 7px 8px; border-radius: 8px; }
  .mrow:hover { background: color-mix(in srgb, var(--ink) 4%, transparent); }
  .mnm { font-size: 12.5px; font-weight: 600; font-family: var(--mono); }
  .mds { grid-column: 1; font-size: 11px; color: var(--ink-soft); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .inst { grid-column: 2; grid-row: 1 / span 2; padding: 5px 12px; border-radius: 8px; border: 1px solid color-mix(in srgb, var(--accent) 45%, var(--hairline)); background: transparent; color: var(--accent-strong, var(--accent)); font-size: 11px; font-weight: 600; }
  .inst:hover { background: color-mix(in srgb, var(--accent) 12%, transparent); }
  .inst.on { border-color: var(--hairline); color: var(--ink-soft); }
  .inst.on:hover { border-color: color-mix(in srgb, var(--danger) 40%, var(--hairline)); color: var(--danger); background: color-mix(in srgb, var(--danger) 8%, transparent); }

  .new { display: flex; flex-direction: column; gap: 6px; margin-top: 6px; }
  input { background: color-mix(in srgb, var(--ink) 4%, var(--surface)); border: 1px solid var(--hairline); border-radius: var(--radius-control); padding: 8px 11px; color: var(--ink); font-size: 12px; font-family: var(--mono); outline: none; transition: border-color .15s ease, box-shadow .15s ease; }
  input:focus { border-color: color-mix(in srgb, var(--accent) 55%, var(--hairline)); box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 16%, transparent); }
  .add { text-align: center; padding: 7px 10px; border: 1px dashed var(--hairline); background: transparent; border-radius: 8px; color: var(--ink-soft); font-size: 12px; transition: border-color .14s ease, color .14s ease; }
  .add:hover:not(:disabled) { border-color: color-mix(in srgb, var(--accent) 45%, var(--hairline)); color: var(--accent-strong); }
  .add:disabled { opacity: 0.45; }

  .editor { display: flex; flex-direction: column; min-width: 0; }
  .ehead { display: flex; align-items: center; gap: 8px; padding: 10px 14px; border-bottom: 1px solid var(--hairline); }
  .dt { font-size: 13px; font-weight: 600; font-family: var(--mono); }
  .robadge { font-size: 11px; color: var(--ink-faint); }
  .spacer { flex: 1; }
  .dirty { color: var(--accent); font-size: 10px; }
  .primary { background: linear-gradient(135deg, var(--grad-a), var(--grad-b)); color: #fff; border: none; border-radius: 9px; padding: 7px 15px; font-size: 12px; font-weight: 550; box-shadow: 0 3px 12px color-mix(in srgb, var(--grad-a) 40%, transparent); transition: transform .12s ease, box-shadow .14s ease, opacity .14s ease; }
  .primary:hover:not(:disabled) { transform: translateY(-1px); box-shadow: 0 6px 18px color-mix(in srgb, var(--grad-a) 50%, transparent); }
  .primary:disabled { opacity: 0.45; }
  textarea.code { flex: 1; resize: none; border: none; outline: none; background: var(--code-bg); color: var(--ink); padding: 12px 14px; font-family: var(--mono); font-size: 12.5px; line-height: 1.6; }
  textarea.code[readonly] { opacity: .85; }

  .err { position: fixed; bottom: 12px; left: 50%; transform: translateX(-50%); background: var(--surface); border: 1px solid color-mix(in srgb, var(--danger) 50%, var(--hairline)); color: var(--danger); padding: 8px 14px; border-radius: 8px; font-size: 12px; box-shadow: var(--shadow-pop); }
  .confirm-scrim { position: absolute; inset: 0; z-index: 10; background: color-mix(in srgb, var(--ink) 30%, transparent); display: grid; place-items: center; }
  .confirm { width: min(360px, 80%); background: var(--surface); border: 1px solid var(--hairline); border-radius: var(--radius); box-shadow: var(--shadow-pop); padding: 18px; }
  .confirm p { margin: 0 0 14px; font-size: 13px; }
  .cbtns { display: flex; justify-content: flex-end; gap: 8px; }
  .cbtns button { padding: 7px 14px; border-radius: var(--radius-control); border: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 4%, var(--surface)); color: var(--ink); font-size: 12px; }
  .cbtns .danger { background: var(--danger); border-color: var(--danger); color: #fff; }
</style>
