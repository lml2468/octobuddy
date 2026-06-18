<script lang="ts">
  import { XClawService } from "../../../bindings/github.com/lml2468/xclaw/desktop";

  let { onclose, onedit, onusage }: { onclose: () => void; onedit?: () => void; onusage?: () => void } = $props();

  type SkillInfo = { name: string; description: string; files: number };

  const isPreview = new URLSearchParams(location.search).has("preview");

  let skills = $state<SkillInfo[]>([]);
  let sel = $state<string | null>(null);
  let files = $state<string[]>([]);
  let activeFile = $state<string | null>(null);
  let content = $state("");
  let dirty = $state(false);
  let error = $state("");
  let newName = $state("");
  let newFilePath = $state("");
  // window.confirm() is a no-op in the Wails webview, so use an in-app dialog.
  let confirmState = $state<{ msg: string; resolve: (v: boolean) => void } | null>(null);
  function ask(msg: string): Promise<boolean> {
    return new Promise((resolve) => (confirmState = { msg, resolve }));
  }
  function answer(v: boolean) {
    confirmState?.resolve(v);
    confirmState = null;
  }

  // Preview-mode in-memory catalog so the layout can be screenshotted without a daemon.
  const mock: Record<string, Record<string, string>> = {
    "pdf-tools": {
      "SKILL.md": "---\nname: pdf-tools\ndescription: Extract text and fill forms in PDF files.\n---\n\n# pdf-tools\n\nUse for reading and filling PDFs.",
      "scripts/extract.py": "import sys\nprint('extract', sys.argv)",
    },
    "octo-broadcast": {
      "SKILL.md": "---\nname: octo-broadcast\ndescription: Send an announcement to every channel the bot is in.\n---\n\n# octo-broadcast\n\nCall octo-cli to broadcast.",
    },
  };

  load();

  async function load() {
    error = "";
    try {
      if (isPreview) {
        skills = Object.entries(mock).map(([name, fs]) => ({
          name, description: descOf(fs["SKILL.md"] ?? ""), files: Object.keys(fs).length,
        }));
      } else {
        skills = ((await XClawService.SkillsList()) ?? []) as SkillInfo[];
      }
      if (skills.length && !sel) selectSkill(skills[0].name);
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  function descOf(skillmd: string): string {
    const m = skillmd.match(/^description:\s*(.+)$/m);
    return m ? m[1].replace(/^["']|["']$/g, "").trim() : "";
  }

  async function selectSkill(name: string) {
    sel = name; activeFile = null; content = ""; dirty = false; error = "";
    try {
      files = isPreview ? Object.keys(mock[name] ?? {}).sort() : (((await XClawService.SkillFiles(name)) ?? []) as string[]);
      const first = files.find((f) => f === "SKILL.md") ?? files[0];
      if (first) openFile(first);
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function openFile(rel: string) {
    if (dirty && !(await ask("Discard unsaved changes?"))) return;
    activeFile = rel; error = "";
    try {
      content = isPreview ? (mock[sel!]?.[rel] ?? "") : await XClawService.SkillRead(sel!, rel);
      dirty = false;
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function saveFile() {
    if (!sel || !activeFile) return;
    try {
      if (isPreview) { (mock[sel] ??= {})[activeFile] = content; }
      else await XClawService.SkillWrite(sel, activeFile, content);
      dirty = false;
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function addFile() {
    const rel = newFilePath.trim();
    if (!sel || !rel) return;
    try {
      if (isPreview) { (mock[sel] ??= {})[rel] = ""; }
      else await XClawService.SkillWrite(sel, rel, "");
      newFilePath = "";
      await selectSkill(sel);
      openFile(rel);
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function deleteFile(rel: string) {
    if (!sel || rel === "SKILL.md") return;
    if (!(await ask(`Delete ${rel}?`))) return;
    try {
      if (isPreview) { delete mock[sel][rel]; }
      else await XClawService.SkillDeleteFile(sel, rel);
      if (activeFile === rel) { activeFile = null; content = ""; }
      await selectSkill(sel);
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function createSkill() {
    const name = newName.trim();
    if (!name) return;
    try {
      if (isPreview) { mock[name] = { "SKILL.md": `---\nname: ${name}\ndescription: One line on when to use this skill.\n---\n\n# ${name}\n` }; }
      else await XClawService.SkillCreate(name);
      newName = "";
      await load();
      selectSkill(name);
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function deleteSkill(name: string) {
    if (!(await ask(`Delete the skill "${name}" and all its files?`))) return;
    try {
      if (isPreview) { delete mock[name]; } else await XClawService.SkillDelete(name);
      if (sel === name) { sel = null; files = []; activeFile = null; content = ""; }
      await load();
    } catch (e: any) { error = String(e?.message ?? e); }
  }
</script>

<div class="scrim" onclick={onclose} role="presentation">
  <div class="modal" onclick={(e) => e.stopPropagation()} role="dialog" aria-label="Manage skills">
    <header>
      <h2>设置</h2>
      <div class="nav" role="tablist" aria-label="设置分区">
        <button role="tab" aria-selected="false" onclick={() => { onclose(); onedit?.(); }}>编辑 Bot</button>
        <button role="tab" aria-selected="true" class="on">技能</button>
        <button role="tab" aria-selected="false" onclick={() => { onclose(); onusage?.(); }}>用量</button>
      </div>
      <span class="hspacer"></span>
      <button class="x" onclick={onclose} aria-label="关闭">✕</button>
    </header>

    <div class="body">
    <div class="list">
      {#each skills as s (s.name)}
        <button class="row" class:sel={s.name === sel} onclick={() => selectSkill(s.name)}>
          <span class="nm">{s.name}</span>
          <span class="ds">{s.description || "No description"}</span>
        </button>
      {/each}
      {#if skills.length === 0}<div class="muted">暂无技能</div>{/if}
      <div class="new">
        <input placeholder="新技能名称" bind:value={newName} onkeydown={(e) => e.key === "Enter" && createSkill()} />
        <button class="add" onclick={createSkill} disabled={!newName.trim()}>+ 新建技能</button>
      </div>
    </div>

    {#if sel}
      <div class="detail">
        <div class="dhead">
          <span class="dt">{sel}</span>
          <span class="spacer"></span>
          <button class="remove" onclick={() => deleteSkill(sel!)}>删除技能</button>
        </div>
        <div class="cols">
          <div class="files">
            {#each files as f (f)}
              <div class="frow" class:sel={f === activeFile}>
                <button class="fname" onclick={() => openFile(f)}>{f}</button>
                {#if f !== "SKILL.md"}<button class="del" title="Delete file" onclick={() => deleteFile(f)}>−</button>{/if}
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
              <div class="muted center">选择一个文件编辑</div>
            {/if}
          </div>
        </div>
      </div>
    {:else}
      <div class="detail"><div class="muted center">选择或新建一个技能</div></div>
    {/if}
  </div>

  {#if error}<div class="err">⚠️ {error}</div>{/if}

  {#if confirmState}
    <div class="confirm-scrim" role="presentation">
      <div class="confirm" role="alertdialog" aria-label="Confirm">
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
  /* Mirrors ConfigEditor (Edit Bots): same scrim + centered modal + header/✕,
     so the two open and feel identical. */
  .scrim { position: fixed; inset: 0; z-index: 50; background: var(--window-grad); display: block; }
  .modal { width: 100%; height: 100%; position: relative; display: flex; flex-direction: column; background: var(--glass); backdrop-filter: blur(24px) saturate(180%); -webkit-backdrop-filter: blur(24px) saturate(180%); border: none; border-radius: 0; box-shadow: none; overflow: hidden; color: var(--ink); font-family: var(--ui); }
  header { display: flex; align-items: center; gap: 12px; padding: 14px 18px 14px 92px; -webkit-app-region: drag; border-bottom: 1px solid var(--border-soft, var(--hairline)); }
  header .seg, header .nav, header .x, header .seg button, header .nav button { -webkit-app-region: no-drag; }
  header h2 { font-size: 17px; font-weight: 600; }
  .hspacer { flex: 1; }
  .nav { display: inline-flex; background: rgba(var(--ink-tint, 0,0,0), 0.05); border-radius: 10px; padding: 3px; }
  .nav button { padding: 6px 14px; border: none; background: transparent; border-radius: 8px; font-size: 13px; color: var(--ink-soft); cursor: pointer; }
  .nav button.on { background: var(--surface); color: var(--ink); box-shadow: var(--elev-1, 0 1px 2px rgba(0,0,0,0.08)); }
  .nav button:not(.on):hover { color: var(--ink); }
  header h2 { font-size: 17px; font-weight: 600; flex: 1; }
  .x { background: none; border: none; color: var(--ink-soft); font-size: 15px; }

  .body { flex: 1; display: grid; grid-template-columns: 220px 1fr; overflow: hidden; }

  .list { border-right: 1px solid var(--hairline); padding: 10px; display: flex; flex-direction: column; gap: 3px; overflow-y: auto; background: color-mix(in srgb, var(--ink) 3%, transparent); }
  .row { display: flex; flex-direction: column; gap: 2px; text-align: left; padding: 8px 10px; border: none; background: transparent; border-radius: 4px; color: var(--ink); }
  .row:hover { background: color-mix(in srgb, var(--ink) 5%, transparent); }
  .row.sel { background: color-mix(in srgb, var(--accent) 16%, transparent); }
  .row .nm { font-size: 13px; font-weight: 600; }
  .row .ds { font-size: 11px; color: var(--ink-soft); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .muted { color: var(--ink-faint); font-size: 12px; padding: 12px; }
  .muted.center { display: grid; place-items: center; height: 100%; }

  .new { display: flex; flex-direction: column; gap: 6px; margin-top: 6px; }
  input { background: color-mix(in srgb, var(--ink) 4%, var(--surface)); border: 1px solid var(--hairline); border-radius: 10px; padding: 8px 11px; color: var(--ink); font-size: 12px; font-family: var(--mono); outline: none; transition: border-color .15s ease, box-shadow .15s ease; }
  input:focus { border-color: color-mix(in srgb, var(--accent) 55%, var(--hairline)); box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 16%, transparent); }
  .add { text-align: center; padding: 7px 10px; border: 1px dashed var(--hairline); background: transparent; border-radius: 4px; color: var(--ink-soft); font-size: 12px; }
  .add:hover:not(:disabled) { border-color: color-mix(in srgb, var(--accent) 45%, var(--hairline)); color: var(--accent-strong); }
  .add:disabled { opacity: 0.45; }

  .detail { display: flex; flex-direction: column; min-width: 0; overflow: hidden; }
  .dhead { display: flex; align-items: center; padding: 12px 16px; border-bottom: 1px solid var(--hairline); }
  .dt { font-size: 14px; font-weight: 600; font-family: var(--mono); }
  .spacer { flex: 1; }
  .remove { color: var(--danger); background: transparent; border: 1px solid color-mix(in srgb, var(--danger) 40%, var(--hairline)); border-radius: 4px; padding: 5px 11px; font-size: 12px; }

  .cols { flex: 1; display: grid; grid-template-columns: 210px 1fr; min-height: 0; }
  .files { border-right: 1px solid var(--hairline); padding: 10px; display: flex; flex-direction: column; gap: 2px; overflow-y: auto; background: color-mix(in srgb, var(--ink) 3%, transparent); }
  .frow { display: flex; align-items: center; border-radius: 4px; }
  .frow:hover { background: color-mix(in srgb, var(--ink) 5%, transparent); }
  .frow.sel { background: color-mix(in srgb, var(--accent) 16%, transparent); }
  .fname { flex: 1; min-width: 0; text-align: left; background: transparent; border: none; color: var(--ink); padding: 7px 9px; font-size: 12px; font-family: var(--mono); overflow: hidden; text-overflow: ellipsis; }
  .del { width: 24px; height: 24px; border: none; background: transparent; color: var(--ink-faint); font-size: 15px; }
  .del:hover { color: var(--danger); }

  .editor { display: flex; flex-direction: column; min-width: 0; }
  .ebar { display: flex; align-items: center; gap: 8px; padding: 10px 14px; border-bottom: 1px solid var(--hairline); }
  .ebar .fn { font-size: 12px; font-family: var(--mono); color: var(--ink-soft); }
  .ebar .dirty { color: var(--accent); font-size: 10px; }
  .primary { background: var(--accent); color: #fff; border: 1px solid var(--accent); border-radius: 4px; padding: 6px 14px; font-size: 12px; }
  .primary:disabled { opacity: 0.45; }
  textarea.code { flex: 1; resize: none; border: none; outline: none; background: var(--code-bg); color: var(--ink); padding: 12px 14px; font-family: var(--mono); font-size: 12.5px; line-height: 1.6; }

  .err { position: fixed; bottom: 12px; left: 50%; transform: translateX(-50%); background: var(--surface); border: 1px solid color-mix(in srgb, var(--danger) 50%, var(--hairline)); color: var(--danger); padding: 8px 14px; border-radius: 8px; font-size: 12px; box-shadow: var(--shadow-pop); }

  .confirm-scrim { position: absolute; inset: 0; z-index: 10; background: color-mix(in srgb, var(--ink) 30%, transparent); display: grid; place-items: center; }
  .confirm { width: min(360px, 80%); background: var(--surface); border: 1px solid var(--hairline); border-radius: var(--radius); box-shadow: var(--shadow-pop); padding: 18px; }
  .confirm p { margin: 0 0 14px; font-size: 13px; color: var(--ink); }
  .cbtns { display: flex; justify-content: flex-end; gap: 8px; }
  .cbtns button { padding: 7px 14px; border-radius: 4px; border: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 4%, var(--surface)); color: var(--ink); font-size: 12px; }
  .cbtns .danger { background: var(--danger); border-color: var(--danger); color: #fff; }
</style>
