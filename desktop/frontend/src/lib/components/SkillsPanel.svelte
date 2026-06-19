<script lang="ts">
  import { XClawService } from "../../../bindings/github.com/lml2468/xclaw/desktop";
  import { store } from "../store.svelte";
  import { modal } from "../actions/modal";
  import SettingsHeader from "./SettingsHeader.svelte";

  let { onclose, onedit, onusage, onworkflows }: { onclose: () => void; onedit?: () => void; onusage?: () => void; onworkflows?: () => void } = $props();

  type SkillInfo = { name: string; description: string; files: number; installed: boolean; broken?: boolean };

  const isPreview = new URLSearchParams(location.search).has("preview");

  // The bot whose skills we manage. Defaults to the selected bot; a picker switches.
  let botId = $state<string | null>(store.selectedBotId ?? store.bots[0]?.id ?? null);

  // If the panel opened before the bot roster loaded (botId null), adopt the
  // first bot once it arrives, then load its skills — without clobbering an
  // explicit picker choice.
  $effect(() => {
    if (botId == null && store.bots.length) {
      botId = store.selectedBotId ?? store.bots[0].id;
      if (isPreview) loadBotPreview(); else loadBot();
    }
  });

  let botSkills = $state<SkillInfo[]>([]); // this bot's own + installed skills
  let catalog = $state<SkillInfo[]>([]);   // global marketplace catalog
  let sel = $state<string | null>(null);   // selected bot skill (for the editor)
  let files = $state<string[]>([]);
  let activeFile = $state<string | null>(null);
  let content = $state("");
  let dirty = $state(false);
  let error = $state("");
  let newName = $state("");
  let newFilePath = $state("");

  // The selected skill's "installed" flag → its files are read-only marketplace content.
  const selInstalled = $derived(botSkills.find((s) => s.name === sel)?.installed ?? false);
  const installedNames = $derived(new Set(botSkills.filter((s) => s.installed).map((s) => s.name)));

  // window.confirm() is a no-op in the Wails webview, so use an in-app dialog.
  let confirmState = $state<{ msg: string; resolve: (v: boolean) => void } | null>(null);
  function ask(msg: string): Promise<boolean> {
    return new Promise((resolve) => (confirmState = { msg, resolve }));
  }
  function answer(v: boolean) { confirmState?.resolve(v); confirmState = null; }
  async function leave(fn?: () => void) {
    if (dirty && !(await ask("有未保存的改动,确认离开?"))) return;
    onclose(); fn?.();
  }

  // Preview-mode in-memory state so the layout can be screenshotted without a daemon.
  const mockCatalog: Record<string, { description: string; files: Record<string, string> }> = {
    "pdf-tools": { description: "Extract text and fill forms in PDF files.", files: { "SKILL.md": "---\nname: pdf-tools\ndescription: Extract text and fill forms in PDF files.\n---\n\n# pdf-tools" } },
    "octo-broadcast": { description: "Send an announcement to every channel.", files: { "SKILL.md": "---\nname: octo-broadcast\ndescription: Send an announcement to every channel.\n---\n\n# octo-broadcast" } },
  };
  // bot id → { name → {installed, files} }
  const mockBot: Record<string, Record<string, { installed: boolean; files: Record<string, string> }>> = {
    main: {
      "pdf-tools": { installed: true, files: mockCatalog["pdf-tools"].files },
      "my-helper": { installed: false, files: { "SKILL.md": "---\nname: my-helper\ndescription: This bot's own helper skill.\n---\n\n# my-helper" } },
    },
    research: {},
  };

  load();

  async function load() {
    error = "";
    try {
      if (isPreview) {
        catalog = Object.entries(mockCatalog).map(([name, c]) => ({ name, description: c.description, files: Object.keys(c.files).length, installed: false }));
        loadBotPreview();
        return;
      }
      catalog = ((await XClawService.SkillsList()) ?? []) as SkillInfo[];
      await loadBot();
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  function loadBotPreview() {
    const b = botId ?? "";
    botSkills = Object.entries(mockBot[b] ?? {}).map(([name, s]) => ({
      name, description: descOf(s.files["SKILL.md"] ?? ""), files: Object.keys(s.files).length, installed: s.installed,
    }));
    if (botSkills.length && !botSkills.find((s) => s.name === sel)) selectSkill(botSkills[0].name);
    else if (!botSkills.length) { sel = null; files = []; activeFile = null; content = ""; }
  }

  async function loadBot() {
    if (!botId) { botSkills = []; sel = null; files = []; return; }
    botSkills = ((await XClawService.BotSkillsList(botId)) ?? []) as SkillInfo[];
    if (botSkills.length && !botSkills.find((s) => s.name === sel)) selectSkill(botSkills[0].name);
    else if (!botSkills.length) { sel = null; files = []; activeFile = null; content = ""; }
  }

  async function switchBot(id: string) {
    if (dirty && !(await ask("放弃未保存的改动?"))) return;
    botId = id; sel = null; activeFile = null; content = ""; dirty = false;
    if (isPreview) loadBotPreview(); else await loadBot();
  }

  function descOf(skillmd: string): string {
    const m = skillmd.match(/^description:\s*(.+)$/m);
    return m ? m[1].replace(/^["']|["']$/g, "").trim() : "";
  }

  async function selectSkill(name: string) {
    sel = name; activeFile = null; content = ""; dirty = false; error = "";
    try {
      if (isPreview) {
        files = Object.keys(mockBot[botId ?? ""]?.[name]?.files ?? {}).sort();
      } else {
        files = ((await XClawService.BotSkillFiles(botId!, name)) ?? []) as string[];
      }
      const first = files.find((f) => f === "SKILL.md") ?? files[0];
      if (first) openFile(first);
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function openFile(rel: string) {
    if (dirty && !(await ask("放弃未保存的改动?"))) return;
    activeFile = rel; error = "";
    try {
      content = isPreview ? (mockBot[botId ?? ""]?.[sel!]?.files[rel] ?? "") : await XClawService.BotSkillRead(botId!, sel!, rel);
      dirty = false;
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function saveFile() {
    if (!sel || !activeFile || selInstalled) return;
    try {
      if (isPreview) { (mockBot[botId ?? ""][sel].files)[activeFile] = content; }
      else await XClawService.BotSkillWrite(botId!, sel, activeFile, content);
      dirty = false;
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function addFile() {
    const rel = newFilePath.trim();
    if (!sel || !rel || selInstalled) return;
    try {
      if (isPreview) { (mockBot[botId ?? ""][sel].files)[rel] = ""; }
      else await XClawService.BotSkillWrite(botId!, sel, rel, "");
      newFilePath = "";
      await selectSkill(sel);
      openFile(rel);
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function deleteFile(rel: string) {
    if (!sel || rel === "SKILL.md" || selInstalled) return;
    if (!(await ask(`删除文件 ${rel}?`))) return;
    try {
      if (isPreview) { delete mockBot[botId ?? ""][sel].files[rel]; }
      else await XClawService.BotSkillDeleteFile(botId!, sel, rel);
      if (activeFile === rel) { activeFile = null; content = ""; }
      await selectSkill(sel);
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function createOwn() {
    const name = newName.trim();
    if (!name || !botId) return;
    try {
      if (isPreview) { (mockBot[botId] ??= {})[name] = { installed: false, files: { "SKILL.md": `---\nname: ${name}\ndescription: One line on when to use this skill.\n---\n\n# ${name}\n` } }; loadBotPreview(); }
      else { await XClawService.BotSkillCreate(botId, name); await loadBot(); }
      newName = "";
      selectSkill(name);
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function removeBotSkill(s: SkillInfo) {
    const verb = s.installed ? "卸载" : "删除";
    if (!(await ask(`${verb}「${s.name}」?`))) return;
    try {
      if (isPreview) { delete mockBot[botId ?? ""][s.name]; loadBotPreview(); }
      else {
        if (s.installed) await XClawService.BotSkillUninstall(botId!, s.name);
        else await XClawService.BotSkillDelete(botId!, s.name);
        await loadBot();
      }
      if (sel === s.name) { sel = null; files = []; activeFile = null; content = ""; }
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function install(name: string) {
    if (!botId) return;
    try {
      if (isPreview) { (mockBot[botId] ??= {})[name] = { installed: true, files: mockCatalog[name].files }; loadBotPreview(); }
      else { await XClawService.BotSkillInstall(botId, name); await loadBot(); }
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function uninstall(name: string) {
    if (!botId) return;
    try {
      if (isPreview) { delete mockBot[botId ?? ""][name]; loadBotPreview(); }
      else { await XClawService.BotSkillUninstall(botId, name); await loadBot(); }
      if (sel === name) { sel = null; files = []; activeFile = null; content = ""; }
    } catch (e: any) { error = String(e?.message ?? e); }
  }
</script>

<div class="scrim" onclick={() => leave()} role="presentation">
  <!-- svelte-ignore a11y_click_events_have_key_events (use:modal handles Escape/Tab; this onclick only stops propagation) -->
  <div class="modal" use:modal={{ onclose: () => leave() }} onclick={(e) => e.stopPropagation()} role="dialog" aria-label="技能" tabindex="-1">
    <SettingsHeader active="skills" onclose={() => leave()} onnav={leave} {onedit} {onusage} {onworkflows}>
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
        <div class="sectlbl">本 Bot 技能</div>
        {#each botSkills as s (s.name)}
          <div class="row" class:sel={s.name === sel}>
            <button class="rowmain" onclick={() => (s.broken ? null : selectSkill(s.name))} disabled={s.broken}>
              <span class="nm">{s.name}{#if s.broken}<span class="badge broken">失效</span>{:else if s.installed}<span class="badge">已安装</span>{:else}<span class="badge own">自有</span>{/if}</span>
              <span class="ds">{s.broken ? "市场来源已删除,卸载以清理" : (s.description || "无描述")}</span>
            </button>
            <button class="del" title={s.installed ? "卸载" : "删除"} onclick={() => removeBotSkill(s)}>−</button>
          </div>
        {/each}
        {#if botSkills.length === 0}<div class="muted">该 Bot 还没有技能</div>{/if}
        <div class="new">
          <input placeholder="新建自有技能名称" bind:value={newName} onkeydown={(e) => e.key === "Enter" && createOwn()} />
          <button class="add" onclick={createOwn} disabled={!newName.trim()}>+ 新建自有技能</button>
        </div>

        <div class="sectlbl market">技能市场</div>
        {#each catalog as c (c.name)}
          <div class="mrow">
            <span class="mnm">{c.name}</span>
            <span class="mds">{c.description || "无描述"}</span>
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
      <div class="detail">
        <div class="dhead">
          <span class="dt">{sel}</span>
          {#if selInstalled}<span class="robadge">市场技能 · 只读</span>{/if}
          <span class="spacer"></span>
        </div>
        <div class="cols">
          <div class="files">
            {#each files as f (f)}
              <div class="frow" class:sel={f === activeFile}>
                <button class="fname" onclick={() => openFile(f)}>{f}</button>
                {#if f !== "SKILL.md" && !selInstalled}<button class="del" title="删除文件" onclick={() => deleteFile(f)}>−</button>{/if}
              </div>
            {/each}
            {#if !selInstalled}
              <div class="new">
                <input placeholder="路径/文件.ext" bind:value={newFilePath} onkeydown={(e) => e.key === "Enter" && addFile()} />
                <button class="add" onclick={addFile} disabled={!newFilePath.trim()}>+ 添加文件</button>
              </div>
            {/if}
          </div>
          <div class="editor">
            {#if activeFile}
              <div class="ebar">
                <span class="fn">{activeFile}</span>
                <span class="spacer"></span>
                {#if dirty}<span class="dirty">●</span>{/if}
                {#if !selInstalled}<button class="primary" onclick={saveFile} disabled={!dirty}>保存</button>{/if}
              </div>
              <textarea class="code" bind:value={content} oninput={() => (dirty = true)} readonly={selInstalled} spellcheck="false"></textarea>
            {:else}
              <div class="muted center">选择一个文件查看</div>
            {/if}
          </div>
        </div>
      </div>
    {:else}
      <div class="detail"><div class="muted center">选择一个技能,或从市场安装</div></div>
    {/if}
  </div>

  {#if error}<div class="err">⚠️ {error}</div>{/if}

  {#if confirmState}
    <div class="confirm-scrim" role="presentation">
      <div class="confirm" role="alertdialog" aria-label="Confirm" tabindex="-1">
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
  /* Mirrors ConfigEditor: full-window scrim + glass modal + shared SettingsHeader. */
  .scrim { position: fixed; inset: 0; z-index: 50; background: var(--window-grad); display: block; }
  .modal { width: 100%; height: 100%; position: relative; display: flex; flex-direction: column; background: var(--glass); backdrop-filter: blur(24px) saturate(180%); -webkit-backdrop-filter: blur(24px) saturate(180%); border: none; border-radius: 0; box-shadow: none; overflow: hidden; color: var(--ink); font-family: var(--ui); }
  .row:focus-visible, .rowmain:focus-visible, .add:focus-visible, .primary:focus-visible, .fname:focus-visible, .del:focus-visible, .inst:focus-visible, .cbtns button:focus-visible { outline: none; box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 30%, transparent); }
  .add:hover:not(:disabled) { border-color: color-mix(in srgb, var(--accent) 45%, var(--hairline)); color: var(--accent-strong, var(--accent)); }

  .botpick { display: inline-flex; align-items: center; gap: 7px; font-size: 12px; color: var(--ink-soft); -webkit-app-region: no-drag; }
  .botpick select { background: color-mix(in srgb, var(--ink) 5%, var(--surface)); border: 1px solid var(--hairline); border-radius: 8px; padding: 5px 9px; color: var(--ink); font-size: 12px; font-family: var(--mono); outline: none; }

  .body { flex: 1; display: grid; grid-template-columns: 260px 1fr; overflow: hidden; }

  .list { border-right: 1px solid var(--hairline); padding: 10px; display: flex; flex-direction: column; gap: 3px; overflow-y: auto; background: color-mix(in srgb, var(--ink) 3%, transparent); }
  .sectlbl { font-size: 11px; font-weight: 600; color: var(--ink-faint); text-transform: uppercase; letter-spacing: .04em; padding: 6px 6px 3px; }
  .sectlbl.market { margin-top: 12px; border-top: 1px solid var(--hairline); padding-top: 12px; }
  .row { display: flex; align-items: center; border-radius: 8px; }
  .row:hover { background: color-mix(in srgb, var(--ink) 5%, transparent); }
  .row.sel { background: color-mix(in srgb, var(--accent) 16%, transparent); }
  .rowmain { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; text-align: left; padding: 8px 10px; border: none; background: transparent; color: var(--ink); }
  .nm { font-size: 13px; font-weight: 600; display: flex; align-items: center; gap: 6px; }
  .badge { font-size: 10px; font-weight: 600; padding: 1px 6px; border-radius: 6px; background: color-mix(in srgb, var(--accent) 18%, transparent); color: var(--accent-strong, var(--accent)); }
  .badge.own { background: color-mix(in srgb, var(--ink) 10%, transparent); color: var(--ink-soft); }
  .badge.broken { background: color-mix(in srgb, var(--danger) 16%, transparent); color: var(--danger); }
  .rowmain:disabled { cursor: default; opacity: .7; }
  .ds { font-size: 11px; color: var(--ink-soft); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
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
  input { background: color-mix(in srgb, var(--ink) 4%, var(--surface)); border: 1px solid var(--hairline); border-radius: 10px; padding: 8px 11px; color: var(--ink); font-size: 12px; font-family: var(--mono); outline: none; transition: border-color .15s ease, box-shadow .15s ease; }
  input:focus { border-color: color-mix(in srgb, var(--accent) 55%, var(--hairline)); box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 16%, transparent); }
  .add { text-align: center; padding: 7px 10px; border: 1px dashed var(--hairline); background: transparent; border-radius: 8px; color: var(--ink-soft); font-size: 12px; }
  .add:hover:not(:disabled) { border-color: color-mix(in srgb, var(--accent) 45%, var(--hairline)); color: var(--accent-strong); }
  .add:disabled { opacity: 0.45; }

  .detail { display: flex; flex-direction: column; min-width: 0; overflow: hidden; }
  .dhead { display: flex; align-items: center; gap: 10px; padding: 12px 16px; border-bottom: 1px solid var(--hairline); }
  .dt { font-size: 14px; font-weight: 600; font-family: var(--mono); }
  .robadge { font-size: 11px; color: var(--ink-faint); }
  .spacer { flex: 1; }

  .cols { flex: 1; display: grid; grid-template-columns: 210px 1fr; min-height: 0; }
  .files { border-right: 1px solid var(--hairline); padding: 10px; display: flex; flex-direction: column; gap: 2px; overflow-y: auto; background: color-mix(in srgb, var(--ink) 3%, transparent); }
  .frow { display: flex; align-items: center; border-radius: 8px; }
  .frow:hover { background: color-mix(in srgb, var(--ink) 5%, transparent); }
  .frow.sel { background: color-mix(in srgb, var(--accent) 16%, transparent); }
  .fname { flex: 1; min-width: 0; text-align: left; background: transparent; border: none; color: var(--ink); padding: 7px 9px; font-size: 12px; font-family: var(--mono); overflow: hidden; text-overflow: ellipsis; }
  .del { width: 24px; height: 24px; border: none; background: transparent; color: var(--ink-faint); font-size: 15px; }
  .del:hover { color: var(--danger); }

  .editor { display: flex; flex-direction: column; min-width: 0; }
  .ebar { display: flex; align-items: center; gap: 8px; padding: 10px 14px; border-bottom: 1px solid var(--hairline); }
  .ebar .fn { font-size: 12px; font-family: var(--mono); color: var(--ink-soft); }
  .ebar .dirty { color: var(--accent); font-size: 10px; }
  .primary { background: linear-gradient(135deg, var(--grad-a), var(--grad-b)); color: #fff; border: none; border-radius: 9px; padding: 7px 15px; font-size: 12px; font-weight: 550; box-shadow: 0 3px 12px color-mix(in srgb, var(--grad-a) 40%, transparent); transition: transform .12s ease, box-shadow .14s ease, opacity .14s ease; }
  .primary:hover:not(:disabled) { transform: translateY(-1px); box-shadow: 0 6px 18px color-mix(in srgb, var(--grad-a) 50%, transparent); }
  .primary:disabled { opacity: 0.45; }
  textarea.code { flex: 1; resize: none; border: none; outline: none; background: var(--code-bg); color: var(--ink); padding: 12px 14px; font-family: var(--mono); font-size: 12.5px; line-height: 1.6; }
  textarea.code[readonly] { opacity: .85; }

  .err { position: fixed; bottom: 12px; left: 50%; transform: translateX(-50%); background: var(--surface); border: 1px solid color-mix(in srgb, var(--danger) 50%, var(--hairline)); color: var(--danger); padding: 8px 14px; border-radius: 8px; font-size: 12px; box-shadow: var(--shadow-pop); }

  .confirm-scrim { position: absolute; inset: 0; z-index: 10; background: color-mix(in srgb, var(--ink) 30%, transparent); display: grid; place-items: center; }
  .confirm { width: min(360px, 80%); background: var(--surface); border: 1px solid var(--hairline); border-radius: var(--radius); box-shadow: var(--shadow-pop); padding: 18px; }
  .confirm p { margin: 0 0 14px; font-size: 13px; color: var(--ink); }
  .cbtns { display: flex; justify-content: flex-end; gap: 8px; }
  .cbtns button { padding: 7px 14px; border-radius: var(--radius-control); border: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 4%, var(--surface)); color: var(--ink); font-size: 12px; }
  .cbtns .danger { background: var(--danger); border-color: var(--danger); color: #fff; }
</style>
