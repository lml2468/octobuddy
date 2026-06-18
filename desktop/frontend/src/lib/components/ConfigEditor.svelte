<script lang="ts">
  import { XClawService } from "../../../bindings/github.com/lml2468/xclaw/desktop";
  import { BotConfig } from "../../../bindings/github.com/lml2468/xclaw/desktop/internal/configstore/models";
  import { store } from "../store.svelte";
  import Avatar from "./Avatar.svelte";

  let { onclose, onskills, onusage }: { onclose: () => void; onskills?: () => void; onusage?: () => void } = $props();

  let bots = $state<BotConfig[]>([]);
  let sel = $state(0);
  let error = $state("");
  let saved = $state(false);
  let busy = $state(false);

  const current = $derived(bots[sel] ?? null);
  // Env as an editable list of pairs (kept in sync with current.env on edit).
  let envRows = $state<{ k: string; v: string }[]>([]);
  // Bot ids present when this editor opened — the basis for an EXPLICIT removal
  // list on save (so the daemon never infers deletions from a set-difference).
  let loadedIds: string[] = [];
  // Global skill catalog (for the per-bot available-skills checklist).
  let allSkills = $state<{ name: string; description: string }[]>([]);
  let allWorkflows = $state<{ name: string; description: string }[]>([]);

  function skillOn(name: string): boolean {
    return (current?.skills ?? []).includes(name);
  }
  function toggleSkill(name: string) {
    if (!current) return;
    const set = new Set(current.skills ?? []);
    set.has(name) ? set.delete(name) : set.add(name);
    current.skills = [...set];
  }
  function workflowOn(name: string): boolean {
    return (current?.workflows ?? []).includes(name);
  }
  function toggleWorkflow(name: string) {
    if (!current) return;
    const set = new Set(current.workflows ?? []);
    set.has(name) ? set.delete(name) : set.add(name);
    current.workflows = [...set];
  }

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
        new BotConfig({ id: "main", apiUrl: "https://im.example.com/api", model: "claude-opus-4-8", gatewayBaseUrl: "https://gw.example/v1", env: { OCTO_BOT_ID: "main-7f3a" }, soul: "You are Atlas, the team's ops copilot.", agents: "Confirm before destructive actions.", skills: ["pdf-tools"] }),
        new BotConfig({ id: "research", apiUrl: "https://im.example.com/api" }),
      ];
      allSkills = [{ name: "pdf-tools", description: "Extract text and fill PDF forms." }, { name: "octo-broadcast", description: "Announce to every channel." }];
      allWorkflows = [{ name: "review-changes", description: "Multi-dimension diff review." }, { name: "deep-audit", description: "Exhaustive audit pass." }];
      sel = 0;
      return;
    }
    try {
      bots = (await XClawService.LoadConfig()) ?? [];
      loadedIds = bots.map((b) => b.id);
      try { allSkills = ((await XClawService.SkillsList()) ?? []) as any; } catch { allSkills = []; }
      try { allWorkflows = ((await XClawService.WorkflowsList()) ?? []) as any; } catch { allWorkflows = []; }
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
      if (restart) { await XClawService.RestartCore(); store.bots = []; XClawService.BotsList(); onclose(); }
    } catch (e: any) {
      error = String(e?.message ?? e);
    } finally {
      busy = false;
    }
  }
</script>

<div class="scrim" onclick={onclose} role="presentation">
  <div class="modal" onclick={(e) => e.stopPropagation()} role="dialog" aria-label="Edit bots">
    <header>
      <h2>设置</h2>
      <div class="seg" role="tablist" aria-label="设置分区">
        <button role="tab" aria-selected="true" class="on">编辑 Bot</button>
        <button role="tab" aria-selected="false" onclick={() => { onclose(); onskills?.(); }}>技能</button>
        <button role="tab" aria-selected="false" onclick={() => { onclose(); onusage?.(); }}>用量</button>
      </div>
      <span class="hspacer"></span>
      <button class="x" onclick={onclose} aria-label="关闭">✕</button>
    </header>

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
        <div class="form">
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
            <span class="lbl">可用技能</span>
            {#if allSkills.length === 0}
              <small>技能库还是空的 — 从「管理技能」里添加。</small>
            {:else}
              {#each allSkills as s (s.name)}
                <label class="skrow">
                  <input type="checkbox" checked={skillOn(s.name)} onchange={() => toggleSkill(s.name)} />
                  <span class="sknm">{s.name}</span>
                  <span class="skds">{s.description}</span>
                </label>
              {/each}
            {/if}
          </div>

          <div class="skills">
            <span class="lbl">可用工作流</span>
            {#if allWorkflows.length === 0}
              <small>工作流库还是空的 — 从「管理工作流」里添加。</small>
            {:else}
              {#each allWorkflows as w (w.name)}
                <label class="skrow">
                  <input type="checkbox" checked={workflowOn(w.name)} onchange={() => toggleWorkflow(w.name)} />
                  <span class="sknm">{w.name}</span>
                  <span class="skds">{w.description}</span>
                </label>
              {/each}
            {/if}
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
  </div>
</div>

<style>
  .scrim { position: fixed; inset: 0; z-index: 50; background: var(--window-grad); display: block; }
  .modal { width: 100%; height: 100%; display: flex; flex-direction: column; background: var(--glass); backdrop-filter: blur(24px) saturate(180%); -webkit-backdrop-filter: blur(24px) saturate(180%); border: none; border-radius: 0; box-shadow: none; overflow: hidden; }
  header { display: flex; align-items: center; gap: 12px; padding: 14px 18px 14px 92px; border-bottom: 1px solid var(--border-soft, var(--hairline)); -webkit-app-region: drag; }
  header .seg, header .nav, header .x { -webkit-app-region: no-drag; }
  header h2 { font-size: 17px; font-weight: 600; }
  .hspacer { flex: 1; }
  .seg { display: inline-flex; background: rgba(var(--ink-tint, 0,0,0), 0.05); border-radius: 10px; padding: 3px; }
  .seg button { padding: 6px 14px; border: none; background: transparent; border-radius: 8px; font-size: 13px; color: var(--ink-soft); cursor: pointer; }
  .seg button.on { background: var(--surface); color: var(--ink); box-shadow: var(--elev-1, 0 1px 2px rgba(0,0,0,0.08)); }
  .seg button:not(.on):hover { color: var(--ink); }
  .x { background: none; border: none; color: var(--ink-soft); font-size: 15px; }

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
  label { display: flex; flex-direction: column; gap: 5px; font-size: 12px; color: var(--ink-soft); }
  input, textarea { background: color-mix(in srgb, var(--ink) 4%, var(--surface)); border: 1px solid var(--hairline); border-radius: 10px; padding: 8px 11px; color: var(--ink); font-size: 13px; outline: none; transition: border-color .15s ease, box-shadow .15s ease; }
  input:focus, textarea:focus { border-color: color-mix(in srgb, var(--accent) 55%, var(--hairline)); box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 16%, transparent); }
  textarea { resize: vertical; font-family: var(--ui); }
  small { color: var(--ink-faint); font-size: 11px; }

  .env { display: flex; flex-direction: column; gap: 6px; }
  .lbl { font-size: 12px; color: var(--ink-soft); }
  .skills { display: flex; flex-direction: column; gap: 5px; }
  .skills small { color: var(--ink-faint); font-size: 11px; }
  .skrow { display: flex; flex-direction: row; align-items: center; gap: 8px; padding: 4px 6px; border-radius: 5px; }
  .skrow:hover { background: color-mix(in srgb, var(--ink) 4%, transparent); }
  .skrow input { accent-color: var(--accent); margin: 0; flex: 0 0 auto; }
  .skrow .sknm { font-family: var(--mono); font-size: 12px; color: var(--ink); flex: 0 0 auto; }
  .skrow .skds { font-size: 11px; color: var(--ink-soft); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .envrow { display: flex; align-items: center; gap: 6px; }
  .envrow .k { width: 160px; font-family: var(--mono); font-size: 12px; }
  .envrow .v { flex: 1; }
  .del { width: 26px; height: 26px; border-radius: 3px; border: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 4%, var(--surface)); color: var(--ink-soft); }
  .remove { align-self: flex-start; margin-top: 6px; color: var(--danger); background: transparent; border: 1px solid color-mix(in srgb, var(--danger) 40%, var(--hairline)); border-radius: 4px; padding: 6px 12px; }

  footer { display: flex; align-items: center; gap: 10px; padding: 12px 18px; border-top: 1px solid var(--hairline); }
  .spacer { flex: 1; }
  footer button { padding: 7px 16px; border-radius: 4px; border: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 4%, var(--surface)); color: var(--ink); }
  footer .primary { background: var(--accent); color: #fff; border-color: var(--accent); }
  .err { color: var(--danger); font-size: 12px; }
  .ok { color: #5aa873; font-size: 12px; }
</style>
