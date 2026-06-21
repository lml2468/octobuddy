<script lang="ts">
  // Per-bot Workflows pane: lifted from the old WorkflowsPanel body. Like
  // SkillsPane, the Settings modal owns bot selection + scaffolding; this is
  // just the script list + editor for one bot. Writes through to disk.
  import { XClawService } from "../../../bindings/github.com/lml2468/xclaw/desktop";
  import { confirm } from "../confirm.svelte";
  import { errMsg } from "../errors";
  import ErrorFooter from "./ErrorFooter.svelte";

  let { botId, isPreview = false }: { botId: string; isPreview?: boolean } = $props();

  type WfInfo = { name: string; description: string };

  let wfs = $state<WfInfo[]>([]);
  let sel = $state<string | null>(null);
  let content = $state("");
  let dirty = $state(false);
  let error = $state("");
  let newName = $state("");

  const mockBot: Record<string, Record<string, string>> = {
    main: {
      "review-changes": "export const meta = {\n  name: 'review-changes',\n  description: 'Review the diff across dimensions and verify each finding.',\n  phases: [{ title: 'Review' }, { title: 'Verify' }],\n}\nphase('Review')\nreturn { ok: true }\n",
    },
    research: {},
  };

  $effect(() => { botId; sel = null; content = ""; dirty = false; load(); });

  async function load() {
    error = "";
    try {
      if (isPreview) {
        wfs = Object.entries(mockBot[botId] ?? {}).map(([name, src]) => ({ name, description: descOf(src) }));
      } else {
        wfs = ((await XClawService.BotWorkflowsList(botId)) ?? []) as WfInfo[];
      }
      if (wfs.length && !wfs.find((w) => w.name === sel)) select(wfs[0].name);
      else if (!wfs.length) { sel = null; content = ""; }
    } catch (e) { error = errMsg(e); }
  }

  function descOf(src: string): string {
    const m = src.match(/description\s*:\s*["']([^"']+)["']/);
    return m ? m[1] : "";
  }

  async function select(name: string) {
    if (dirty && !(await confirm({ message: "放弃未保存的改动?", confirmLabel: "放弃", danger: true }))) return;
    sel = name; error = "";
    try {
      content = isPreview ? (mockBot[botId]?.[name] ?? "") : await XClawService.BotWorkflowRead(botId, name);
      dirty = false;
    } catch (e) { error = errMsg(e); }
  }

  async function save() {
    if (!sel) return;
    try {
      if (isPreview) { mockBot[botId][sel] = content; }
      else await XClawService.BotWorkflowWrite(botId, sel, content);
      dirty = false;
    } catch (e) { error = errMsg(e); }
  }

  async function createOwn() {
    const name = newName.trim();
    if (!name) return;
    try {
      if (isPreview) { (mockBot[botId] ??= {})[name] = `export const meta = {\n  name: '${name}',\n  description: 'One line on what this workflow does.',\n}\nreturn { ok: true }\n`; load(); }
      else { await XClawService.BotWorkflowCreate(botId, name); await load(); }
      newName = "";
      select(name);
    } catch (e) { error = errMsg(e); }
  }

  async function remove(w: WfInfo) {
    if (!(await confirm({ message: `删除「${w.name}」?`, confirmLabel: "删除", danger: true }))) return;
    try {
      if (isPreview) { delete mockBot[botId][w.name]; load(); }
      else { await XClawService.BotWorkflowDelete(botId, w.name); await load(); }
      if (sel === w.name) { sel = null; content = ""; dirty = false; }
    } catch (e) { error = errMsg(e); }
  }
</script>

<div class="pane">
  <aside class="list">
    {#each wfs as w (w.name)}
      <div class="row" class:sel={w.name === sel}>
        <button class="rowmain" onclick={() => select(w.name)}>
          <span class="nm">{w.name}</span>
          <span class="ds">{w.description || "暂无描述"}</span>
        </button>
        <button class="del" title="删除" onclick={() => remove(w)}>−</button>
      </div>
    {/each}
    {#if wfs.length === 0}<div class="muted">该 Bot 还没有工作流</div>{/if}
    <div class="new">
      <input placeholder="新建工作流名称" bind:value={newName} onkeydown={(e) => e.key === "Enter" && createOwn()} />
      <button class="add" onclick={createOwn} disabled={!newName.trim()}>+ 新建工作流</button>
    </div>
  </aside>

  {#if sel}
    <section class="editor">
      <div class="ebar">
        <span class="dt">{sel}.js</span>
        <span class="spacer"></span>
        {#if dirty}<span class="dirty">●</span>{/if}
        <button class="primary" onclick={save} disabled={!dirty}>保存</button>
      </div>
      <textarea class="code" bind:value={content} oninput={() => (dirty = true)} spellcheck="false"></textarea>
    </section>
  {:else}
    <section class="editor"><div class="muted center">选择或新建一个工作流</div></section>
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
  .nm { font-size: 13px; font-weight: 600; font-family: var(--mono); }
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

  .editor { display: flex; flex-direction: column; min-width: 0; border: 1px solid var(--hairline); border-radius: 12px; overflow: hidden; }
  .ebar { display: flex; align-items: center; gap: 8px; padding: 8px 12px; border-bottom: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 3%, transparent); }
  .dt { font-size: 13px; font-weight: 600; font-family: var(--mono); }
  .spacer { flex: 1; }
  .dirty { color: var(--accent); font-size: 10px; }
  .primary { background: linear-gradient(135deg, var(--grad-a), var(--grad-b)); color: #fff; border: none; border-radius: 8px; padding: 6px 13px; font-size: 12px; font-weight: 550; box-shadow: 0 3px 12px color-mix(in srgb, var(--grad-a) 40%, transparent); }
  .primary:hover:not(:disabled) { transform: translateY(-1px); box-shadow: 0 6px 18px color-mix(in srgb, var(--grad-a) 50%, transparent); }
  .primary:disabled { opacity: 0.45; cursor: default; }
  textarea.code { flex: 1; resize: none; border: none; outline: none; background: var(--code-bg); color: var(--ink); padding: 12px 14px; font-family: var(--mono); font-size: 12.5px; line-height: 1.6; }
</style>
