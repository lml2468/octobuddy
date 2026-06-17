<script lang="ts">
  import { XClawService } from "../../../bindings/github.com/lml2468/xclaw/desktop";

  let { onclose }: { onclose: () => void } = $props();

  type WfInfo = { name: string; description: string };
  const isPreview = new URLSearchParams(location.search).has("preview");

  let list = $state<WfInfo[]>([]);
  let sel = $state<string | null>(null);
  let content = $state("");
  let dirty = $state(false);
  let error = $state("");
  let newName = $state("");
  // window.confirm() is a no-op in the Wails webview — use an in-app dialog.
  let confirmState = $state<{ msg: string; resolve: (v: boolean) => void } | null>(null);
  function ask(msg: string): Promise<boolean> {
    return new Promise((resolve) => (confirmState = { msg, resolve }));
  }
  function answer(v: boolean) { confirmState?.resolve(v); confirmState = null; }

  const mock: Record<string, string> = {
    "review-changes": "export const meta = {\n  name: 'review-changes',\n  description: 'Review the diff across dimensions and verify each finding.',\n  phases: [{ title: 'Review' }, { title: 'Verify' }],\n}\nphase('Review')\nreturn { ok: true }\n",
    "deep-audit": "export const meta = {\n  name: 'deep-audit',\n  description: 'Exhaustive multi-pass audit with adversarial verification.',\n}\nreturn { ok: true }\n",
  };

  load();

  async function load() {
    error = "";
    try {
      if (isPreview) {
        list = Object.entries(mock).map(([name, src]) => ({ name, description: descOf(src) }));
      } else {
        list = ((await XClawService.WorkflowsList()) ?? []) as WfInfo[];
      }
      if (list.length && !sel) select(list[0].name);
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  function descOf(src: string): string {
    const m = src.match(/description\s*:\s*["']([^"']+)["']/);
    return m ? m[1] : "";
  }

  async function select(name: string) {
    if (dirty && !(await ask("Discard unsaved changes?"))) return;
    sel = name; error = "";
    try {
      content = isPreview ? (mock[name] ?? "") : await XClawService.WorkflowRead(name);
      dirty = false;
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function save() {
    if (!sel) return;
    try {
      if (isPreview) { mock[sel] = content; } else await XClawService.WorkflowWrite(sel, content);
      dirty = false;
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function create() {
    const name = newName.trim();
    if (!name) return;
    try {
      if (isPreview) { mock[name] = `export const meta = {\n  name: '${name}',\n  description: 'One line on what this workflow does.',\n}\nreturn { ok: true }\n`; }
      else await XClawService.WorkflowCreate(name);
      newName = "";
      await load();
      select(name);
    } catch (e: any) { error = String(e?.message ?? e); }
  }

  async function remove(name: string) {
    if (!(await ask(`Delete the workflow "${name}"?`))) return;
    try {
      if (isPreview) { delete mock[name]; } else await XClawService.WorkflowDelete(name);
      if (sel === name) { sel = null; content = ""; }
      await load();
    } catch (e: any) { error = String(e?.message ?? e); }
  }
</script>

<div class="scrim" onclick={onclose} role="presentation">
  <div class="modal" onclick={(e) => e.stopPropagation()} role="dialog" aria-label="Manage workflows">
    <header><h2>Manage Workflows</h2><button class="x" onclick={onclose} aria-label="Close">✕</button></header>

    <div class="body">
      <div class="list">
        {#each list as w (w.name)}
          <button class="row" class:sel={w.name === sel} onclick={() => select(w.name)}>
            <span class="nm">{w.name}</span>
            <span class="ds">{w.description || "No description"}</span>
          </button>
        {/each}
        {#if list.length === 0}<div class="muted">No workflows yet.</div>{/if}
        <div class="new">
          <input placeholder="new-workflow-name" bind:value={newName} onkeydown={(e) => e.key === "Enter" && create()} />
          <button class="add" onclick={create} disabled={!newName.trim()}>+ New workflow</button>
        </div>
      </div>

      {#if sel}
        <div class="editor">
          <div class="ehead">
            <span class="dt">{sel}.js</span>
            <span class="spacer"></span>
            {#if dirty}<span class="dirty">●</span>{/if}
            <button class="primary" onclick={save} disabled={!dirty}>Save</button>
            <button class="remove" onclick={() => remove(sel!)}>Delete</button>
          </div>
          <textarea class="code" bind:value={content} oninput={() => (dirty = true)} spellcheck="false"></textarea>
        </div>
      {:else}
        <div class="editor"><div class="muted center">Select or create a workflow.</div></div>
      {/if}
    </div>

    {#if error}<div class="err">⚠️ {error}</div>{/if}

    {#if confirmState}
      <div class="confirm-scrim" role="presentation">
        <div class="confirm" role="alertdialog" aria-label="Confirm">
          <p>{confirmState.msg}</p>
          <div class="cbtns">
            <button onclick={() => answer(false)}>Cancel</button>
            <button class="danger" onclick={() => answer(true)}>Confirm</button>
          </div>
        </div>
      </div>
    {/if}
  </div>
</div>

<style>
  /* Mirrors ConfigEditor / SkillsPanel: scrim + centered modal + header/✕. */
  .scrim { position: fixed; inset: 0; z-index: 50; background: color-mix(in srgb, var(--ink) 28%, transparent); display: grid; place-items: center; }
  .modal { width: min(940px, 94vw); height: min(640px, 90vh); position: relative; display: flex; flex-direction: column; background: var(--surface); border: 1px solid var(--hairline); border-radius: var(--radius); box-shadow: var(--shadow-pop); overflow: hidden; color: var(--ink); font-family: var(--ui); }
  header { display: flex; align-items: center; padding: 16px 18px; border-bottom: 1px solid var(--hairline); }
  header h2 { font-size: 17px; font-weight: 600; flex: 1; }
  .x { background: none; border: none; color: var(--ink-soft); font-size: 15px; }

  .body { flex: 1; display: grid; grid-template-columns: 240px 1fr; overflow: hidden; }
  .list { border-right: 1px solid var(--hairline); padding: 10px; display: flex; flex-direction: column; gap: 3px; overflow-y: auto; background: color-mix(in srgb, var(--ink) 3%, transparent); }
  .row { display: flex; flex-direction: column; gap: 2px; text-align: left; padding: 8px 10px; border: none; background: transparent; border-radius: 4px; color: var(--ink); }
  .row:hover { background: color-mix(in srgb, var(--ink) 5%, transparent); }
  .row.sel { background: color-mix(in srgb, var(--accent) 16%, transparent); }
  .row .nm { font-size: 13px; font-weight: 600; font-family: var(--mono); }
  .row .ds { font-size: 11px; color: var(--ink-soft); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .muted { color: var(--ink-faint); font-size: 12px; padding: 12px; }
  .muted.center { display: grid; place-items: center; height: 100%; }
  .new { display: flex; flex-direction: column; gap: 6px; margin-top: 6px; }
  input { background: color-mix(in srgb, var(--ink) 4%, var(--surface)); border: 1px solid var(--hairline); border-radius: 4px; padding: 7px 10px; color: var(--ink); font-size: 12px; font-family: var(--mono); outline: none; }
  input:focus { border-color: color-mix(in srgb, var(--accent) 55%, var(--hairline)); }
  .add { text-align: center; padding: 7px 10px; border: 1px dashed var(--hairline); background: transparent; border-radius: 4px; color: var(--ink-soft); font-size: 12px; }
  .add:hover:not(:disabled) { border-color: color-mix(in srgb, var(--accent) 45%, var(--hairline)); color: var(--accent-strong); }
  .add:disabled { opacity: 0.45; }

  .editor { display: flex; flex-direction: column; min-width: 0; }
  .ehead { display: flex; align-items: center; gap: 8px; padding: 10px 14px; border-bottom: 1px solid var(--hairline); }
  .dt { font-size: 13px; font-weight: 600; font-family: var(--mono); }
  .spacer { flex: 1; }
  .dirty { color: var(--accent); font-size: 10px; }
  .primary { background: var(--accent); color: #fff; border: 1px solid var(--accent); border-radius: 4px; padding: 6px 14px; font-size: 12px; }
  .primary:disabled { opacity: 0.45; }
  .remove { color: var(--danger); background: transparent; border: 1px solid color-mix(in srgb, var(--danger) 40%, var(--hairline)); border-radius: 4px; padding: 6px 11px; font-size: 12px; }
  textarea.code { flex: 1; resize: none; border: none; outline: none; background: var(--code-bg); color: var(--ink); padding: 12px 14px; font-family: var(--mono); font-size: 12.5px; line-height: 1.6; }

  .err { position: fixed; bottom: 12px; left: 50%; transform: translateX(-50%); background: var(--surface); border: 1px solid color-mix(in srgb, var(--danger) 50%, var(--hairline)); color: var(--danger); padding: 8px 14px; border-radius: 8px; font-size: 12px; box-shadow: var(--shadow-pop); }
  .confirm-scrim { position: absolute; inset: 0; z-index: 10; background: color-mix(in srgb, var(--ink) 30%, transparent); display: grid; place-items: center; }
  .confirm { width: min(360px, 80%); background: var(--surface); border: 1px solid var(--hairline); border-radius: var(--radius); box-shadow: var(--shadow-pop); padding: 18px; }
  .confirm p { margin: 0 0 14px; font-size: 13px; }
  .cbtns { display: flex; justify-content: flex-end; gap: 8px; }
  .cbtns button { padding: 7px 14px; border-radius: 4px; border: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 4%, var(--surface)); color: var(--ink); font-size: 12px; }
  .cbtns .danger { background: var(--danger); border-color: var(--danger); color: #fff; }
</style>
