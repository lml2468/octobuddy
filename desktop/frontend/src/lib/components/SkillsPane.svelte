<script lang="ts">
  // Per-bot Skills pane: lifted from the old SkillsPanel body. The Settings
  // modal owns bot selection + scaffolding; this is just the bundle list +
  // file editor for one bot. Writes through to disk immediately (not part
  // of the basic/octo "save" dirty flag).
  import { XClawService } from "../../../bindings/github.com/lml2468/xclaw/desktop";
  import { confirm } from "../confirm.svelte";
  import { errMsg } from "../errors";
  import ErrorFooter from "./ErrorFooter.svelte";

  let { botId, isPreview = false }: { botId: string; isPreview?: boolean } = $props();

  type SkillInfo = { name: string; description: string; files: number };

  let skills = $state<SkillInfo[]>([]);
  let sel = $state<string | null>(null);
  let files = $state<string[]>([]);
  let activeFile = $state<string | null>(null);
  let content = $state("");
  let dirty = $state(false);
  let error = $state("");
  let newName = $state("");
  let newFilePath = $state("");

  const mockBot: Record<string, Record<string, Record<string, string>>> = {
    main: {
      "my-helper": { "SKILL.md": "---\nname: my-helper\ndescription: This bot's own helper skill.\n---\n\n# my-helper" },
      "pdf-tools": { "SKILL.md": "---\nname: pdf-tools\ndescription: Extract text and fill forms in PDF files.\n---\n\n# pdf-tools" },
    },
    research: {},
  };

  $effect(() => { botId; sel = null; files = []; activeFile = null; content = ""; dirty = false; load(); });

  async function load() {
    error = "";
    try {
      if (isPreview) {
        skills = Object.entries(mockBot[botId] ?? {}).map(([name, fs]) => ({
          name, description: descOf(fs["SKILL.md"] ?? ""), files: Object.keys(fs).length,
        }));
      } else {
        skills = ((await XClawService.BotSkillsList(botId)) ?? []) as SkillInfo[];
      }
      if (skills.length && !skills.find((s) => s.name === sel)) selectSkill(skills[0].name);
      else if (!skills.length) { sel = null; files = []; activeFile = null; content = ""; }
    } catch (e) { error = errMsg(e); }
  }

  function descOf(skillmd: string): string {
    const m = skillmd.match(/^description:\s*(.+)$/m);
    return m ? m[1].replace(/^["']|["']$/g, "").trim() : "";
  }

  async function selectSkill(name: string) {
    sel = name; activeFile = null; content = ""; dirty = false; error = "";
    try {
      files = isPreview
        ? Object.keys(mockBot[botId]?.[name] ?? {}).sort()
        : ((await XClawService.BotSkillFiles(botId, name)) ?? []) as string[];
      const first = files.find((f) => f === "SKILL.md") ?? files[0];
      if (first) openFile(first);
    } catch (e) { error = errMsg(e); }
  }

  async function openFile(rel: string) {
    if (dirty && !(await confirm({ message: "放弃未保存的改动?", confirmLabel: "放弃", danger: true }))) return;
    activeFile = rel; error = "";
    try {
      content = isPreview ? (mockBot[botId]?.[sel!]?.[rel] ?? "") : await XClawService.BotSkillRead(botId, sel!, rel);
      dirty = false;
    } catch (e) { error = errMsg(e); }
  }

  async function saveFile() {
    if (!sel || !activeFile) return;
    try {
      if (isPreview) { (mockBot[botId][sel])[activeFile] = content; }
      else await XClawService.BotSkillWrite(botId, sel, activeFile, content);
      dirty = false;
    } catch (e) { error = errMsg(e); }
  }

  async function addFile() {
    const rel = newFilePath.trim();
    if (!sel || !rel) return;
    try {
      if (isPreview) { (mockBot[botId][sel])[rel] = ""; }
      else await XClawService.BotSkillWrite(botId, sel, rel, "");
      newFilePath = "";
      await selectSkill(sel);
      openFile(rel);
    } catch (e) { error = errMsg(e); }
  }

  async function deleteFile(rel: string) {
    if (!sel || rel === "SKILL.md") return;
    if (!(await confirm({ message: `删除文件 ${rel}?`, confirmLabel: "删除", danger: true }))) return;
    try {
      if (isPreview) { delete mockBot[botId][sel][rel]; }
      else await XClawService.BotSkillDeleteFile(botId, sel, rel);
      if (activeFile === rel) { activeFile = null; content = ""; }
      await selectSkill(sel);
    } catch (e) { error = errMsg(e); }
  }

  async function createOwn() {
    const name = newName.trim();
    if (!name) return;
    try {
      if (isPreview) { (mockBot[botId] ??= {})[name] = { "SKILL.md": `---\nname: ${name}\ndescription: One line on when to use this skill.\n---\n\n# ${name}\n` }; load(); }
      else { await XClawService.BotSkillCreate(botId, name); await load(); }
      newName = "";
      selectSkill(name);
    } catch (e) { error = errMsg(e); }
  }

  async function removeBotSkill(s: SkillInfo) {
    if (!(await confirm({ message: `删除「${s.name}」?`, confirmLabel: "删除", danger: true }))) return;
    try {
      if (isPreview) { delete mockBot[botId][s.name]; load(); }
      else { await XClawService.BotSkillDelete(botId, s.name); await load(); }
      if (sel === s.name) { sel = null; files = []; activeFile = null; content = ""; }
    } catch (e) { error = errMsg(e); }
  }
</script>

<div class="pane">
  <aside class="list">
    {#each skills as s (s.name)}
      <div class="row" class:sel={s.name === sel}>
        <button class="rowmain" onclick={() => selectSkill(s.name)}>
          <span class="nm">{s.name}</span>
          <span class="ds">{s.description || "无描述"}</span>
        </button>
        <button class="del" title="删除" onclick={() => removeBotSkill(s)}>−</button>
      </div>
    {/each}
    {#if skills.length === 0}<div class="muted">该 Bot 还没有技能</div>{/if}
    <div class="new">
      <input placeholder="新建技能名称" bind:value={newName} onkeydown={(e) => e.key === "Enter" && createOwn()} />
      <button class="add" onclick={createOwn} disabled={!newName.trim()}>+ 新建技能</button>
    </div>
  </aside>

  {#if sel}
    <section class="detail">
      <div class="dhead">
        <span class="dt">{sel}</span>
        <span class="spacer"></span>
      </div>
      <div class="cols">
        <div class="files">
          {#each files as f (f)}
            <div class="frow" class:sel={f === activeFile}>
              <button class="fname" onclick={() => openFile(f)}>{f}</button>
              {#if f !== "SKILL.md"}<button class="del" title="删除文件" onclick={() => deleteFile(f)}>−</button>{/if}
            </div>
          {/each}
          <div class="new">
            <input placeholder="路径/文件.ext" bind:value={newFilePath} onkeydown={(e) => e.key === "Enter" && addFile()} />
            <button class="add" onclick={addFile} disabled={!newFilePath.trim()}>+ 添加文件</button>
          </div>
        </div>
        <div class="editor">
          {#if activeFile}
            <div class="ebar">
              <span class="fn">{activeFile}</span>
              <span class="spacer"></span>
              {#if dirty}<span class="dirty">●</span>{/if}
              <button class="primary" onclick={saveFile} disabled={!dirty}>保存</button>
            </div>
            <textarea class="code" bind:value={content} oninput={() => (dirty = true)} spellcheck="false"></textarea>
          {:else}
            <div class="muted center">选择一个文件查看</div>
          {/if}
        </div>
      </div>
    </section>
  {:else}
    <section class="detail"><div class="muted center">选择或新建一个技能</div></section>
  {/if}
</div>

{#if error}<ErrorFooter {error} onclear={() => (error = "")} />{/if}

<style>
  .pane { display: grid; grid-template-columns: 240px 1fr; gap: 14px; height: 100%; min-height: 360px; }
  .list { display: flex; flex-direction: column; gap: 3px; padding: 4px; overflow-y: auto; }
  .row { display: flex; align-items: center; border-radius: 8px; }
  .row:hover { background: color-mix(in srgb, var(--ink) 5%, transparent); }
  .row.sel { background: color-mix(in srgb, var(--accent) 16%, transparent); }
  .rowmain { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; text-align: left; padding: 8px 10px; border: none; background: transparent; color: var(--ink); }
  .nm { font-size: 13px; font-weight: 600; }
  .ds { font-size: 11px; color: var(--ink-soft); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .del { width: 24px; height: 24px; border: none; background: transparent; color: var(--ink-faint); font-size: 15px; }
  .del:hover { color: var(--danger); }
  .muted { color: var(--ink-faint); font-size: 12px; padding: 12px; }
  .muted.center { display: grid; place-items: center; height: 100%; }

  .new { display: flex; flex-direction: column; gap: 6px; margin-top: 6px; }
  input { background: color-mix(in srgb, var(--ink) 4%, var(--surface)); border: 1px solid var(--hairline); border-radius: 10px; padding: 8px 11px; color: var(--ink); font-size: 12px; font-family: var(--mono); outline: none; }
  input:focus { border-color: color-mix(in srgb, var(--accent) 55%, var(--hairline)); box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 16%, transparent); }
  .add { text-align: center; padding: 7px 10px; border: 1px dashed var(--hairline); background: transparent; border-radius: 8px; color: var(--ink-soft); font-size: 12px; }
  .add:hover:not(:disabled) { border-color: color-mix(in srgb, var(--accent) 45%, var(--hairline)); color: var(--accent-strong); }
  .add:disabled { opacity: 0.45; }

  .detail { display: flex; flex-direction: column; min-width: 0; border: 1px solid var(--hairline); border-radius: 12px; overflow: hidden; }
  .dhead { display: flex; align-items: center; gap: 10px; padding: 10px 14px; border-bottom: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 3%, transparent); }
  .dt { font-size: 13px; font-weight: 600; font-family: var(--mono); }
  .spacer { flex: 1; }
  .cols { flex: 1; display: grid; grid-template-columns: 180px 1fr; min-height: 0; }
  .files { border-right: 1px solid var(--hairline); padding: 8px; display: flex; flex-direction: column; gap: 2px; overflow-y: auto; }
  .frow { display: flex; align-items: center; border-radius: 8px; }
  .frow:hover { background: color-mix(in srgb, var(--ink) 5%, transparent); }
  .frow.sel { background: color-mix(in srgb, var(--accent) 16%, transparent); }
  .fname { flex: 1; min-width: 0; text-align: left; background: transparent; border: none; color: var(--ink); padding: 6px 9px; font-size: 12px; font-family: var(--mono); overflow: hidden; text-overflow: ellipsis; }

  .editor { display: flex; flex-direction: column; min-width: 0; }
  .ebar { display: flex; align-items: center; gap: 8px; padding: 8px 12px; border-bottom: 1px solid var(--hairline); }
  .ebar .fn { font-size: 12px; font-family: var(--mono); color: var(--ink-soft); }
  .dirty { color: var(--accent); font-size: 10px; }
  .primary { background: linear-gradient(135deg, var(--grad-a), var(--grad-b)); color: #fff; border: none; border-radius: 8px; padding: 6px 13px; font-size: 12px; font-weight: 550; box-shadow: 0 3px 12px color-mix(in srgb, var(--grad-a) 40%, transparent); }
  .primary:hover:not(:disabled) { transform: translateY(-1px); box-shadow: 0 6px 18px color-mix(in srgb, var(--grad-a) 50%, transparent); }
  .primary:disabled { opacity: 0.45; cursor: default; }
  textarea.code { flex: 1; resize: none; border: none; outline: none; background: var(--code-bg); color: var(--ink); padding: 12px 14px; font-family: var(--mono); font-size: 12.5px; line-height: 1.6; }
</style>
