<script lang="ts">
  import { XClawService } from "../../../bindings/github.com/lml2468/xclaw/desktop";

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
    if (dirty && !confirm("Discard unsaved changes?")) return;
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
    if (!confirm(`Delete ${rel}?`)) return;
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
    if (!confirm(`Delete the skill "${name}" and all its files?`)) return;
    try {
      if (isPreview) { delete mock[name]; } else await XClawService.SkillDelete(name);
      if (sel === name) { sel = null; files = []; activeFile = null; content = ""; }
      await load();
    } catch (e: any) { error = String(e?.message ?? e); }
  }
</script>

<div class="skills">
  <aside class="catalog">
    <header class="ch"><span class="t">Skills</span></header>
    <div class="rows">
      {#each skills as s (s.name)}
        <button class="srow" class:sel={s.name === sel} onclick={() => selectSkill(s.name)}>
          <span class="nm">{s.name}</span>
          <span class="ds">{s.description || "No description"}</span>
          <span class="fc">{s.files} file{s.files === 1 ? "" : "s"}</span>
        </button>
      {/each}
      {#if skills.length === 0}<div class="empty">No skills yet.</div>{/if}
    </div>
    <div class="add">
      <input placeholder="new-skill-name" bind:value={newName} onkeydown={(e) => e.key === "Enter" && createSkill()} />
      <button class="mk" onclick={createSkill} disabled={!newName.trim()}>+ New</button>
    </div>
  </aside>

  <main class="editor">
    {#if sel}
      <header class="eh">
        <span class="t">{sel}</span>
        <span class="spacer"></span>
        <button class="del" onclick={() => deleteSkill(sel!)}>Delete skill</button>
      </header>
      <div class="body">
        <div class="filelist">
          {#each files as f (f)}
            <div class="frow" class:sel={f === activeFile}>
              <button class="fname" onclick={() => openFile(f)}>{f}</button>
              {#if f !== "SKILL.md"}<button class="fx" title="Delete file" onclick={() => deleteFile(f)}>−</button>{/if}
            </div>
          {/each}
          <div class="addfile">
            <input placeholder="path/in/skill.ext" bind:value={newFilePath} onkeydown={(e) => e.key === "Enter" && addFile()} />
            <button onclick={addFile} disabled={!newFilePath.trim()}>+ File</button>
          </div>
        </div>
        <div class="edit">
          {#if activeFile}
            <div class="editbar"><span class="fn">{activeFile}</span><span class="spacer"></span>{#if dirty}<span class="dot">●</span>{/if}<button class="save" onclick={saveFile} disabled={!dirty}>Save</button></div>
            <textarea bind:value={content} oninput={() => (dirty = true)} spellcheck="false"></textarea>
          {:else}
            <div class="placeholder">Select a file to edit.</div>
          {/if}
        </div>
      </div>
    {:else}
      <div class="placeholder big">Select or create a skill.</div>
    {/if}
  </main>

  {#if error}<div class="err">⚠️ {error}</div>{/if}
</div>

<style>
  .skills { display: grid; grid-template-columns: 280px 1fr; height: 100vh; background: var(--chat); color: var(--ink); font-family: var(--ui); }
  .catalog { display: flex; flex-direction: column; border-right: 1px solid var(--hairline); background: var(--list); min-width: 0; }
  .ch, .eh { display: flex; align-items: center; height: 48px; padding: 0 16px; border-bottom: 1px solid var(--hairline); background: var(--surface); }
  .ch .t, .eh .t { font-size: 15px; font-weight: 600; }
  .spacer { flex: 1; }
  .rows { flex: 1; overflow-y: auto; padding: 6px; }
  .srow { display: flex; flex-direction: column; gap: 2px; width: 100%; text-align: left; padding: 9px 11px; border: none; background: transparent; border-radius: 6px; color: var(--ink); }
  .srow:hover { background: color-mix(in srgb, var(--ink) 5%, transparent); }
  .srow.sel { background: color-mix(in srgb, var(--accent) 16%, transparent); }
  .srow .nm { font-size: 13px; font-weight: 600; font-family: var(--mono); }
  .srow .ds { font-size: 12px; color: var(--ink-soft); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .srow .fc { font-size: 11px; color: var(--ink-faint); }
  .empty, .placeholder { color: var(--ink-faint); font-size: 13px; padding: 16px; }
  .placeholder.big { display: grid; place-items: center; height: 100%; }
  .add, .addfile { display: flex; gap: 6px; padding: 8px; border-top: 1px solid var(--hairline); }
  .add input, .addfile input { flex: 1; min-width: 0; background: color-mix(in srgb, var(--ink) 5%, var(--surface)); border: 1px solid var(--hairline); border-radius: 5px; padding: 6px 9px; font-size: 12px; font-family: var(--mono); color: var(--ink); outline: none; }
  .add .mk, .addfile button, .save { background: var(--accent); color: #fff; border: none; border-radius: 5px; padding: 6px 11px; font-size: 12px; }
  .add .mk:disabled, .addfile button:disabled, .save:disabled { opacity: 0.45; }
  .editor { display: flex; flex-direction: column; min-width: 0; }
  .del { background: transparent; border: 1px solid color-mix(in srgb, var(--danger) 40%, var(--hairline)); color: var(--danger); border-radius: 5px; padding: 5px 11px; font-size: 12px; }
  .body { flex: 1; display: grid; grid-template-columns: 220px 1fr; min-height: 0; }
  .filelist { border-right: 1px solid var(--hairline); overflow-y: auto; padding: 6px; display: flex; flex-direction: column; gap: 2px; }
  .frow { display: flex; align-items: center; border-radius: 5px; }
  .frow.sel { background: color-mix(in srgb, var(--accent) 14%, transparent); }
  .fname { flex: 1; min-width: 0; text-align: left; background: transparent; border: none; color: var(--ink); padding: 7px 9px; font-size: 12px; font-family: var(--mono); overflow: hidden; text-overflow: ellipsis; }
  .fx { background: transparent; border: none; color: var(--ink-faint); padding: 0 9px; font-size: 15px; }
  .fx:hover { color: var(--danger); }
  .edit { display: flex; flex-direction: column; min-width: 0; }
  .editbar { display: flex; align-items: center; gap: 8px; padding: 8px 12px; border-bottom: 1px solid var(--hairline); }
  .editbar .fn { font-size: 12px; font-family: var(--mono); color: var(--ink-soft); }
  .editbar .dot { color: var(--accent); font-size: 10px; }
  textarea { flex: 1; resize: none; border: none; outline: none; background: var(--code-bg); color: var(--ink); padding: 12px 14px; font-family: var(--mono); font-size: 12.5px; line-height: 1.6; }
  .err { position: fixed; bottom: 12px; left: 50%; transform: translateX(-50%); background: var(--surface); border: 1px solid color-mix(in srgb, var(--danger) 50%, var(--hairline)); color: var(--danger); padding: 8px 14px; border-radius: 8px; font-size: 12px; box-shadow: var(--shadow-pop); }
</style>
