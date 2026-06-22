<script lang="ts">
  // Basic Info pane: identity (Bot ID, model), gateway (URL/token),
  // non-Octo env vars, and the persona/behavior prompts (SOUL/AGENTS).
  // Octo-specific fields (apiUrl, octoToken, OCTO_BOT_ID) live in the
  // sibling Octo 集成 pane.
  // Env editor: filter out the keys other panes manage. Keep a local pairs
  // array so the user can add/rename keys without weird proxy-mutation
  // gymnastics, then commit back to bot.env on every change. RESERVED_ENV_KEYS
  // is the single source of truth — see lib/reservedEnv.ts.
  import type { BotConfig } from "../../../bindings/github.com/lml2468/xclaw/desktop/internal/configstore/models";
  import { RESERVED_ENV_KEYS } from "../reservedEnv";

  let { bot = $bindable<BotConfig>(), ondirty, ondelete }:
    { bot: BotConfig; ondirty: () => void; ondelete: () => void } = $props();

  // Stable per-row id so Svelte's keyed each preserves DOM nodes when the
  // user deletes a row — without it, every subsequent row's <input> remounts
  // and the user's caret jumps out (round 19 FE #7).
  type Row = { id: number; k: string; v: string };
  let rowSeq = 0;
  function newRow(k: string, v: string): Row { rowSeq += 1; return { id: rowSeq, k, v }; }
  let rows = $state<Row[]>([]);
  $effect(() => {
    bot.id; // re-seed on bot switch
    const e = bot.env ?? {};
    rows = Object.entries(e).filter(([k]) => !RESERVED_ENV_KEYS.has(k)).map(([k, v]) => newRow(k, String(v ?? "")));
  });
  function commitEnv() {
    // Preserve reserved keys (owned by other panes) by passing them through
    // unchanged; rebuild the free-form half from `rows`.
    const next: { [k: string]: string } = {};
    for (const k of RESERVED_ENV_KEYS) {
      const v = bot.env?.[k];
      if (v !== undefined) next[k] = v;
    }
    for (const r of rows) if (r.k.trim()) next[r.k.trim()] = r.v;
    bot.env = next;
    ondirty();
  }
</script>

<div class="pane" oninput={ondirty} onchange={ondirty}>
  <div class="grid2">
    <label>Bot ID <input bind:value={bot.id} placeholder="my-bot" /></label>
    <label>模型 <input bind:value={bot.model} placeholder="claude-opus-4-8" /></label>
  </div>

  <fieldset>
    <legend>模型网关（可选）</legend>
    <label>网关地址 <input bind:value={bot.gatewayBaseUrl} placeholder="https://gateway/v1" /></label>
    <label>网关 Token
      <input type="password" bind:value={bot.gatewayToken} placeholder="sk-…" />
      <small>存于系统钥匙串，绝不写入 config.json。</small>
    </label>
  </fieldset>

  <fieldset>
    <legend>环境变量</legend>
    {#each rows as row, i (row.id)}
      <div class="envrow">
        <input class="k" bind:value={row.k} oninput={commitEnv} placeholder="KEY" aria-label="环境变量名" />
        <span>=</span>
        <input class="v" bind:value={row.v} oninput={commitEnv} placeholder="value" aria-label="环境变量值" />
        <button class="del" onclick={() => { rows = rows.filter((_, x) => x !== i); commitEnv(); }} aria-label="删除">−</button>
      </div>
    {/each}
    <button class="add sm" onclick={() => { rows = [...rows, newRow("", "")]; }}>+ 添加变量</button>
    <small class="hint">OCTO_BOT_ID 在「Octo 集成」中管理，不出现在这里。</small>
  </fieldset>

  <fieldset>
    <legend>SOUL.md（身份）</legend>
    <textarea bind:value={bot.soul} rows="4" placeholder="身份、语气、角色"></textarea>
  </fieldset>

  <fieldset>
    <legend>AGENTS.md（行为规范）</legend>
    <textarea bind:value={bot.agents} rows="4" placeholder="规范、可做与不可做"></textarea>
  </fieldset>

  <div class="danger-row">
    <button class="remove" onclick={ondelete}>删除此 Bot</button>
    <small>下次「保存并重启」时该 Bot 的所有配置与会话数据会被一并清除。</small>
  </div>
</div>

<style>
  .pane { display: flex; flex-direction: column; gap: 14px; }
  .grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
  label { display: flex; flex-direction: column; gap: 5px; font-size: 12px; font-weight: 550; color: var(--ink-soft); }
  fieldset { border: 1px solid var(--hairline); border-radius: 12px; padding: 14px 16px; display: flex; flex-direction: column; gap: 12px; margin: 0; }
  legend { font-size: 11px; font-weight: 600; color: var(--ink-soft); text-transform: uppercase; letter-spacing: 0.04em; padding: 0 6px; }
  input, textarea { background: color-mix(in srgb, var(--ink) 4%, var(--surface)); border: 1px solid var(--hairline); border-radius: 10px; padding: 8px 11px; color: var(--ink); font-size: 13px; outline: none; transition: border-color .15s ease, box-shadow .15s ease; }
  input:focus, textarea:focus { border-color: color-mix(in srgb, var(--accent) 55%, var(--hairline)); box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 16%, transparent); }
  textarea { resize: vertical; font-family: var(--ui); }
  small { color: var(--ink-faint); font-size: 11px; }
  small.hint { padding-top: 4px; }

  .envrow { display: flex; align-items: center; gap: 6px; }
  .envrow .k { width: 160px; font-family: var(--mono); font-size: 12px; }
  .envrow .v { flex: 1; }
  .del { width: 26px; height: 26px; border-radius: 8px; border: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 4%, var(--surface)); color: var(--ink-soft); }
  .add { text-align: center; padding: 7px 10px; border: 1px dashed var(--hairline); background: transparent; border-radius: 9px; color: var(--ink-soft); }
  .add.sm { font-size: 12px; padding: 5px 8px; text-align: left; align-self: flex-start; }
  .add:hover { border-color: color-mix(in srgb, var(--accent) 45%, var(--hairline)); color: var(--accent-strong, var(--accent)); }

  .danger-row { display: flex; align-items: center; gap: 14px; padding-top: 4px; }
  .remove { color: var(--danger); background: transparent; border: 1px solid color-mix(in srgb, var(--danger) 40%, var(--hairline)); border-radius: 6px; padding: 6px 14px; font-size: 12px; font-weight: 550; flex: 0 0 auto; }
  .remove:hover { background: color-mix(in srgb, var(--danger) 10%, transparent); }
  .remove:focus-visible, .del:focus-visible, .add:focus-visible { outline: none; box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 30%, transparent); }
</style>
