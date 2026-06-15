<script lang="ts">
  import { XClawService } from "../../../bindings/github.com/lml2468/xclaw/desktop";
  import { BotConfig } from "../../../bindings/github.com/lml2468/xclaw/desktop/internal/configstore/models";
  import { store } from "../store.svelte";

  let { onclose }: { onclose: () => void } = $props();

  let bots = $state<BotConfig[]>([]);
  let sel = $state(0);
  let error = $state("");
  let saved = $state(false);
  let busy = $state(false);

  const current = $derived(bots[sel] ?? null);
  // Env as an editable list of pairs (kept in sync with current.env on edit).
  let envRows = $state<{ k: string; v: string }[]>([]);

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
      await XClawService.SaveConfig(bots);
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
    <header><h2>Edit Bots</h2><button class="x" onclick={onclose} aria-label="Close">✕</button></header>

    <div class="body">
      <div class="bots">
        {#each bots as b, i (i)}
          <button class="botrow" class:sel={i === sel} onclick={() => (sel = i)}>{b.id || "(unnamed)"}</button>
        {/each}
        <button class="add" onclick={addBot}>+ Add bot</button>
      </div>

      {#if current}
        <div class="form">
          <label>Bot ID <input bind:value={current.id} placeholder="my-bot" /></label>
          <label>API URL <input bind:value={current.apiUrl} placeholder="https://octo-server" /></label>
          <div class="grp">
            <label>Bot token
              <input type="password" bind:value={current.octoToken} placeholder="bf_…" />
              <small>Stored in your OS keychain, never in config.json.</small>
            </label>
          </div>
          <label>Model <input bind:value={current.model} placeholder="claude-opus-4-8" /></label>
          <label>Gateway base URL <input bind:value={current.gatewayBaseUrl} placeholder="https://gateway/v1 (optional)" /></label>
          <label>Gateway token <input type="password" bind:value={current.gatewayToken} placeholder="sk-… (optional)" /></label>

          <div class="env">
            <span class="lbl">Environment</span>
            {#each envRows as row, i (i)}
              <div class="envrow">
                <input class="k" bind:value={row.k} placeholder="KEY" />
                <span>=</span>
                <input class="v" bind:value={row.v} placeholder="value" />
                <button class="del" onclick={() => (envRows = envRows.filter((_, x) => x !== i))} aria-label="Remove">−</button>
              </div>
            {/each}
            <button class="add sm" onclick={() => (envRows = [...envRows, { k: "", v: "" }])}>+ Add var</button>
          </div>

          <label>SOUL.md <textarea bind:value={current.soul} rows="3" placeholder="Who this bot is — identity, voice, role"></textarea></label>
          <label>AGENTS.md <textarea bind:value={current.agents} rows="3" placeholder="How it should behave — norms, do's and don'ts"></textarea></label>

          <button class="remove" onclick={() => removeBot(sel)}>Remove this bot</button>
        </div>
      {/if}
    </div>

    <footer>
      {#if error}<span class="err">⚠️ {error}</span>{:else if saved}<span class="ok">✓ Saved</span>{/if}
      <span class="spacer"></span>
      <button onclick={() => save(false)} disabled={busy}>Save</button>
      <button class="primary" onclick={() => save(true)} disabled={busy}>Save & Restart</button>
    </footer>
  </div>
</div>

<style>
  .scrim { position: fixed; inset: 0; z-index: 50; background: color-mix(in srgb, var(--ink) 28%, transparent); display: grid; place-items: center; }
  .modal { width: min(860px, 92vw); height: min(620px, 88vh); display: flex; flex-direction: column; background: var(--surface); border: 1px solid var(--hairline); border-radius: var(--radius); box-shadow: var(--shadow-pop); overflow: hidden; }
  header { display: flex; align-items: center; padding: 16px 18px; border-bottom: 1px solid var(--hairline); }
  header h2 { font-size: 17px; flex: 1; }
  .x { background: none; border: none; color: var(--ink-soft); font-size: 15px; }

  .body { flex: 1; display: grid; grid-template-columns: 200px 1fr; overflow: hidden; }
  .bots { border-right: 1px solid var(--hairline); padding: 10px; display: flex; flex-direction: column; gap: 3px; overflow-y: auto; background: color-mix(in srgb, var(--ink) 3%, transparent); }
  .botrow { text-align: left; padding: 8px 10px; border: none; background: transparent; border-radius: 8px; color: var(--ink); }
  .botrow:hover { background: color-mix(in srgb, var(--ink) 5%, transparent); }
  .botrow.sel { background: color-mix(in srgb, var(--accent) 16%, transparent); }
  .add { text-align: left; padding: 8px 10px; border: 1px dashed var(--hairline); background: transparent; border-radius: 8px; color: var(--ink-soft); margin-top: 4px; }
  .add.sm { font-size: 12px; padding: 5px 8px; }

  .form { padding: 16px 18px; overflow-y: auto; display: flex; flex-direction: column; gap: 12px; }
  label { display: flex; flex-direction: column; gap: 4px; font-size: 12px; color: var(--ink-soft); }
  input, textarea { background: color-mix(in srgb, var(--ink) 4%, var(--surface)); border: 1px solid var(--hairline); border-radius: 8px; padding: 7px 10px; color: var(--ink); font-size: 13px; outline: none; }
  input:focus, textarea:focus { border-color: color-mix(in srgb, var(--accent) 55%, var(--hairline)); }
  textarea { resize: vertical; font-family: var(--ui); }
  small { color: var(--ink-faint); font-size: 11px; }

  .env { display: flex; flex-direction: column; gap: 6px; }
  .lbl { font-size: 12px; color: var(--ink-soft); }
  .envrow { display: flex; align-items: center; gap: 6px; }
  .envrow .k { width: 160px; font-family: var(--mono); font-size: 12px; }
  .envrow .v { flex: 1; }
  .del { width: 26px; height: 26px; border-radius: 6px; border: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 4%, var(--surface)); color: var(--ink-soft); }
  .remove { align-self: flex-start; margin-top: 6px; color: var(--danger); background: transparent; border: 1px solid color-mix(in srgb, var(--danger) 40%, var(--hairline)); border-radius: 8px; padding: 6px 12px; }

  footer { display: flex; align-items: center; gap: 10px; padding: 12px 18px; border-top: 1px solid var(--hairline); }
  .spacer { flex: 1; }
  footer button { padding: 7px 16px; border-radius: 9px; border: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 4%, var(--surface)); color: var(--ink); }
  footer .primary { background: var(--accent); color: #fff; border-color: var(--accent); }
  .err { color: var(--danger); font-size: 12px; }
  .ok { color: #5aa873; font-size: 12px; }
</style>
