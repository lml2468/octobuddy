<script lang="ts">
 // Basic Info pane: identity (Bot ID, model), gateway (URL/token),
 // non-Octo env vars, and the persona/behavior prompts (SOUL/AGENTS).
 // Octo-specific fields (apiUrl, octoToken, OCTO_BOT_ID) live in the
 // sibling Octo 集成 pane.
 // Env editor: filter out the keys other panes manage. Keep a local pairs
 // array so the user can add/rename keys without weird proxy-mutation
 // gymnastics, then commit back to bot.env on every change. RESERVED_ENV_KEYS
 // is the single source of truth — see lib/reservedEnv.ts.
  import type { BotConfig } from "../../../bindings/github.com/lml2468/octobuddy/desktop/internal/configstore/models";
  import { RESERVED_ENV_KEYS } from "../reservedEnv";
  import { OctoBuddyService } from "../../../bindings/github.com/lml2468/octobuddy/desktop";
  import { store } from "../store.svelte";

  let { bot = $bindable<BotConfig>(), ondirty, ondelete }:
    { bot: BotConfig; ondirty: () => void; ondelete: () => void } = $props();

 // Stable per-row id so Svelte's keyed each preserves DOM nodes when the
 // user deletes a row — without it, every subsequent row's <input> remounts
 // and the user's caret jumps out.
 //
 // `secret` is the UI flag: should this row's value be stored in the secret
 // backend with only a secretRef in config.json? `locked` means the backend has
 // an existing value that the GUI intentionally cannot read; unlocking clears
 // the field so the user can type a replacement.
  type Row = { id: number; k: string; v: string; secret: boolean; locked: boolean };
  let rowSeq = 0;
  function newRow(k: string, v: string, secret?: boolean, locked?: boolean): Row {
    rowSeq += 1;
    return { id: rowSeq, k, v, secret: secret ?? false, locked: locked ?? false };
  }
 // Seed once at mount. The parent (SettingsModal) wraps this pane in
 // `{#key sel}` so a bot switch remounts the whole component — no
 // reactive re-seed is needed, and reading bot.env reactively here
 // would re-fire on every keystroke in the Bot ID input (bot.id is
 // also a dep when read inside an effect with bot.env), silently
 // wiping any uncommitted blank env row.
  let rows = $state<Row[]>(
    Object.entries(bot.env ?? {})
      .filter(([k]) => !RESERVED_ENV_KEYS.has(k))
      .map(([k, v]) => newRow(k, String(v ?? ""), !!bot.secretEnv?.[k], !!bot.secretEnv?.[k] && !v))
  );
  function commitEnv() {
 // Preserve reserved keys (owned by other panes) by passing them through
 // unchanged; rebuild the free-form half from `rows`. Values are written
 // verbatim; secret rows are routed to the backend by SaveConfig and leave
 // only a secretRef in config.json.
    const next: { [k: string]: string } = {};
    const secretNext: { [k: string]: boolean } = {};
    for (const k of RESERVED_ENV_KEYS) {
      const v = bot.env?.[k];
      if (v !== undefined) next[k] = v;
      if (bot.secretEnv?.[k]) secretNext[k] = true;
    }
    for (const r of rows) {
      const k = r.k.trim();
      if (!k) continue;
      next[k] = r.locked ? "" : r.v;
      if (r.secret) secretNext[k] = true;
    }
    bot.env = next;
    bot.secretEnv = secretNext;
    ondirty();
  }
 // unlock starts an edit cycle on a sealed row: wipes the ciphertext (the
 // GUI deliberately cannot decrypt — the renderer's read access is
 // write-only by design) so the user can type a replacement. The next
 // Save stores the replacement in the secret backend.
  function unlock(i: number) {
    rows[i] = { ...rows[i], v: "", secret: true, locked: false };
    commitEnv();
  }
 // toggleSecret flips the row's secret flag. Plain → secret seals current
 // value on Save. Secret → plain on a locked row is a no-op (we can't read
 // the backend value); user should replace or delete the row.
  function toggleSecret(i: number) {
    const r = rows[i];
    if (!r.secret) {
      rows[i] = { ...r, secret: true, locked: false };
      commitEnv();
      return;
    }
    if (!r.locked) {
      rows[i] = { ...r, secret: false };
      commitEnv();
    }
  }

  // --- system-prompt mode (segmented) ---
  // bot is a BotConfig class instance, which Svelte does NOT deep-proxy, so
  // reading bot.* in markup isn't reactive. Mirror the editable agent fields
  // into local $state (seeded once at mount, like the env `rows`) and commit
  // back to bot on every change — same approach the env editor uses.
  let promptMode = $state<string>(bot.systemPromptMode || "minimal");
  function setPromptMode(mode: string) {
    promptMode = mode;
    bot.systemPromptMode = mode === "minimal" ? "" : mode; // "" = default(minimal)
    ondirty();
  }

  // --- setting sources (user always on; project opt-in with warning) ---
  let projectOn = $state<boolean>((bot.settingSources ?? []).includes("project"));
  function toggleProject() {
    projectOn = !projectOn;
    bot.settingSources = projectOn ? ["user", "project"] : ["user"];
    ondirty();
  }

  // --- bot-level tool whitelist ---
  // The picker offers store.toolset.headlessSafe (probed from the binary).
  // scopedTools === null → "use driver default" (all headless-safe). A Set
  // (incl. empty) means the operator scoped the surface.
  const toolset = $derived(store.toolset);
  let scopedTools = $state<Set<string> | null>(
    bot.tools?.default != null ? new Set(bot.tools.default) : null,
  );
  // Selected names that are NOT in the probed set (e.g. mcp__*, or a tool
  // renamed/missing after a claude upgrade). The probed grid can't render them,
  // so surface them as extra removable rows — otherwise they're retained on save
  // but invisible and un-uncheckable in the picker.
  const extraTools = $derived(
    scopedTools
      ? [...scopedTools].filter((t) => !(toolset?.headlessSafe ?? []).includes(t)).sort()
      : [],
  );
  function commitTools() {
    // scopedTools == null → "use driver default": clear the whole policy from the
    // view model. The backend (applyDefaultTools) preserves any per-channel
    // overrides on disk, so dropping bot.tools here only clears the bot-level
    // default. Otherwise persist the operator's actual selection VERBATIM — do
    // NOT intersect with the probed headlessSafe set, so a name absent from the
    // current probe (an mcp__* tool, or one renamed/missing after a claude
    // upgrade, or a transient narrower probe) survives the round-trip. Sort for
    // a stable diff.
    if (scopedTools == null) {
      bot.tools = undefined;
    } else {
      bot.tools = { ...(bot.tools ?? {}), default: [...scopedTools].sort() };
    }
    ondirty();
  }
  function startScoping() {
    scopedTools = new Set(toolset?.headlessSafe ?? []); // all-on, then prune
    commitTools();
  }
  function clearScoping() {
    scopedTools = null;
    commitTools();
  }
  function toggleTool(name: string) {
    if (!scopedTools) return;
    const next = new Set(scopedTools);
    if (next.has(name)) next.delete(name);
    else next.add(name);
    scopedTools = next;
    commitTools();
  }

  // --- MCP servers (.mcp.json) — file-backed, saved immediately (not via dirty) ---
  type MCPHealth = { name: string; status: string; tools: string[] };
  let mcpText = $state("");
  let mcpLoaded = $state(false);
  let mcpError = $state("");
  let mcpBusy = $state(false);
  let mcpHealth = $state<MCPHealth[] | null>(null);
  let mcpHealthNote = $state("");

  async function loadMCP() {
    if (store.preview) { mcpText = ""; mcpLoaded = true; return; }
    try {
      mcpText = (await OctoBuddyService.LoadMCPConfig(bot.id)) ?? "";
    } catch (e) {
      mcpError = String(e);
    }
    mcpLoaded = true;
  }
  // Load once per mount (parent remounts on bot switch via {#key}).
  $effect(() => { if (!mcpLoaded) void loadMCP(); });

  async function saveAndTestMCP() {
    mcpError = ""; mcpBusy = true; mcpHealth = null; mcpHealthNote = "";
    try {
      if (!store.preview) await OctoBuddyService.SaveMCPConfig(bot.id, mcpText);
    } catch (e) {
      mcpError = String(e); mcpBusy = false; return;
    }
    await testMCP();
    mcpBusy = false;
  }

  // testMCP fires mcp.check; the daemon replies on the event stream. We arm a
  // one-shot listener (store exposes the last mcp.check envelope) and poll it.
  async function testMCP() {
    mcpHealth = null; mcpHealthNote = "";
    if (store.preview) {
      mcpHealthNote = "预览模式不连接后台，无法测试。";
      return;
    }
    const before = store.mcpCheckSeq;
    try {
      await OctoBuddyService.CheckMCP(bot.id);
    } catch (e) {
      mcpError = String(e); return;
    }
    // Wait (up to ~65s — the daemon caps the probe at 60s) for the response.
    // Correlation is (seq advanced past our snapshot) AND (botId matches): the
    // SettingsModal is a singleton (App.svelte makes it mutually exclusive with
    // TokenUsage and only one bot is selected), so there is at most one in-flight
    // CheckMCP per bot — a request-id protocol would be over-engineering here.
    for (let i = 0; i < 130; i++) {
      await new Promise((r) => setTimeout(r, 500));
      const res = store.mcpCheck;
      if (res && store.mcpCheckSeq !== before && res.botId === bot.id) {
        if (!res.configured) { mcpHealthNote = "未配置 MCP 服务器。"; mcpHealth = []; }
        else { mcpHealth = res.servers ?? []; }
        return;
      }
    }
    mcpHealthNote = "测试超时，请稍后重试。";
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
    <legend>System Prompt 模式</legend>
    <div class="modeseg">
      <button type="button" class:active={promptMode === "minimal"} onclick={() => setPromptMode("minimal")}>minimal</button>
      <button type="button" class:active={promptMode === "claude_code"} onclick={() => setPromptMode("claude_code")}>claude_code</button>
    </div>
    <small>minimal（默认）：SOUL+AGENTS 替换内置提示词，cwd .claude/ 不加载。claude_code：追加到内置提示词，cwd .claude/ 自动加载。仅当 SOUL 是按内置提示词编写时才用 claude_code。</small>
  </fieldset>

  <fieldset>
    <legend>配置来源（Setting Sources）</legend>
    <label class="chk"><input type="checkbox" checked disabled /> user（始终启用：加载 CLAUDE_CONFIG_DIR 下的每-bot 技能）</label>
    <label class="chk"><input type="checkbox" checked={projectOn} onchange={toggleProject} /> project（加载沙箱 cwd 的 .claude/ 与 CLAUDE.md）</label>
    {#if projectOn}
      <small class="warn">⚠️ 开启 project 会加载 agent 可写的沙箱目录中的指令/技能——群聊中可被不可信用户影响，存在提示词注入风险。仅建议单运营者可信 bot 开启。</small>
    {/if}
  </fieldset>

  <fieldset>
    <legend>可用工具（Bot 级默认）</legend>
    {#if !toolset}
      <small>正在探测 claude 可用工具…（首次安装/升级后生成）</small>
    {:else if !toolset.probed || (toolset.headlessSafe ?? []).length === 0}
      <small>尚未探测到工具集。安装/升级 claude 后将自动填充。</small>
    {:else if scopedTools == null}
      <small>当前使用全部 headless-安全工具（{(toolset.headlessSafe ?? []).length} 个）。</small>
      <button class="add sm" type="button" onclick={startScoping}>限定可用工具…</button>
    {:else}
      <div class="toolgrid">
        {#each toolset.headlessSafe as name (name)}
          <label class="chk"><input type="checkbox" checked={scopedTools.has(name)} onchange={() => toggleTool(name)} /> {name}</label>
        {/each}
        {#each extraTools as name (name)}
          <label class="chk extra"><input type="checkbox" checked onchange={() => toggleTool(name)} /> {name} <span class="tag">未探测</span></label>
        {/each}
      </div>
      <button class="add sm" type="button" onclick={clearScoping}>恢复为全部工具</button>
      <small>未勾选的工具不会提供给该 Bot。「未探测」项（如 mcp__*）不在当前探测集中，但仍会保留并生效。按频道/私聊的细分工具在聊天窗口右上角配置。</small>
    {/if}
  </fieldset>


  <fieldset>
    <legend>环境变量</legend>
    {#each rows as row, i (row.id)}
      <div class="envrow">
        <input class="k" bind:value={row.k} oninput={commitEnv} placeholder="KEY" aria-label="环境变量名" />
        <span>=</span>
        {#if row.secret && row.locked}
          <input class="v locked" value="••••••••" readonly aria-label="加密的环境变量值" />
          <button class="iconbtn" onclick={() => unlock(i)} title="替换为新值（无法查看现有值）" aria-label="替换">✎</button>
        {:else if row.secret}
          <input class="v" type="password" bind:value={row.v} oninput={commitEnv} placeholder="粘贴敏感值，保存后进入 secret backend" aria-label="待保存的敏感环境变量值" />
          <button class="iconbtn" onclick={() => toggleSecret(i)} title="改为明文" aria-label="改为明文">🔓</button>
        {:else}
          <input class="v" bind:value={row.v} oninput={commitEnv} placeholder="value" aria-label="环境变量值" />
          <button class="iconbtn" onclick={() => toggleSecret(i)} title="加密保存（敏感值如 Token）" aria-label="加密">🔒</button>
        {/if}
        <button class="del" onclick={() => { rows = rows.filter((_, x) => x !== i); commitEnv(); }} aria-label="删除">−</button>
      </div>
    {/each}
    <button class="add sm" onclick={() => { rows = [...rows, newRow("", "")]; }}>+ 添加变量</button>
    <small class="hint">🔒 敏感值进入 secret backend；config.json 只保存 secretRef。OCTO_BOT_ID 在「Octo 集成」中管理，不出现在这里。</small>
  </fieldset>

  <fieldset>
    <legend>SOUL.md（身份）</legend>
    <textarea bind:value={bot.soul} rows="4" placeholder="身份、语气、价值观、底线。留空将写入带「Core Truths / Boundaries / Vibe」分节的默认模板。"></textarea>
  </fieldset>

  <fieldset>
    <legend>AGENTS.md（行为规范）</legend>
    <textarea bind:value={bot.agents} rows="4" placeholder="行为规范与红线。留空将写入带「红线 / 群聊何时发言 / 不可信输入」分节的默认模板。"></textarea>
  </fieldset>

  <fieldset>
    <legend>MCP 服务器（.mcp.json）</legend>
    <textarea class="mono" bind:value={mcpText} rows="6" spellcheck="false"
      placeholder={'{\n  "mcpServers": {\n    "my-server": { "command": "npx", "args": ["-y", "@scope/server"] }\n  }\n}'}></textarea>
    <div class="mcp-actions">
      <button class="add sm" type="button" disabled={mcpBusy} onclick={saveAndTestMCP}>保存并测试连接</button>
      <button class="add sm" type="button" disabled={mcpBusy} onclick={testMCP}>仅测试</button>
      {#if mcpBusy}<small>测试中…</small>{/if}
    </div>
    {#if mcpError}<small class="warn">{mcpError}</small>{/if}
    {#if mcpHealthNote}<small>{mcpHealthNote}</small>{/if}
    {#if mcpHealth && mcpHealth.length}
      <div class="mcp-health">
        {#each mcpHealth as s (s.name)}
          <div class="mcp-row">
            <span class="dot" class:ok={s.status === "connected"} class:bad={s.status !== "connected"}></span>
            <span class="mcp-name">{s.name}</span>
            <span class="mcp-status">{s.status === "connected" ? `已连接 · ${s.tools.length} 个工具` : s.status}</span>
          </div>
        {/each}
      </div>
    {/if}
    <small>标准 mcp.json 格式，保存到 ~/.octobuddy/&lt;id&gt;/.claude/.mcp.json，下个回合生效。留空则删除（停用 MCP）。MCP 工具需在上方「可用工具」中包含对应名称或 mcp__* 才会启用。</small>
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
  input.locked { font-family: var(--mono); letter-spacing: 0.1em; color: var(--ink-faint); cursor: default; }
  textarea { resize: vertical; font-family: var(--ui); }
  small { color: var(--ink-faint); font-size: 11px; }
  small.hint { padding-top: 4px; }
  small.warn { color: var(--danger); }

  .modeseg { display: inline-flex; border: 1px solid var(--hairline); border-radius: 9px; overflow: hidden; align-self: flex-start; }
  .modeseg button { padding: 6px 16px; font-size: 12px; font-weight: 550; background: transparent; color: var(--ink-soft); border: none; border-right: 1px solid var(--hairline); }
  .modeseg button:last-child { border-right: none; }
  .modeseg button.active { background: color-mix(in srgb, var(--accent) 16%, transparent); color: var(--accent-strong, var(--accent)); }

  .chk { flex-direction: row; align-items: center; gap: 8px; font-weight: 500; }
  .chk input { width: auto; }

  .toolgrid { display: grid; grid-template-columns: repeat(auto-fill, minmax(150px, 1fr)); gap: 6px 14px; }
  .chk.extra { color: var(--ink-soft); }
  .chk .tag { font-size: 10px; color: var(--ink-faint); border: 1px solid var(--hairline); border-radius: 5px; padding: 0 4px; }

  textarea.mono { font-family: var(--mono); font-size: 12px; }
  .mcp-actions { display: flex; align-items: center; gap: 10px; }
  .mcp-health { display: flex; flex-direction: column; gap: 5px; }
  .mcp-row { display: flex; align-items: center; gap: 8px; font-size: 12px; }
  .mcp-row .dot { width: 8px; height: 8px; border-radius: 50%; flex: 0 0 auto; }
  .mcp-row .dot.ok { background: var(--accent); }
  .mcp-row .dot.bad { background: var(--danger); }
  .mcp-name { font-family: var(--mono); color: var(--ink); }
  .mcp-status { color: var(--ink-faint); }

  .envrow { display: flex; align-items: center; gap: 6px; }
  .envrow .k { width: 160px; font-family: var(--mono); font-size: 12px; }
  .envrow .v { flex: 1; }
  .iconbtn { width: 26px; height: 26px; border-radius: 8px; border: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 4%, var(--surface)); color: var(--ink-soft); font-size: 12px; display: grid; place-items: center; transition: background .14s ease, color .14s ease; }
  .iconbtn:hover { background: color-mix(in srgb, var(--accent) 10%, transparent); color: var(--accent); }
  .del { width: 26px; height: 26px; border-radius: 8px; border: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 4%, var(--surface)); color: var(--ink-soft); }
  .add { text-align: center; padding: 7px 10px; border: 1px dashed var(--hairline); background: transparent; border-radius: 9px; color: var(--ink-soft); }
  .add.sm { font-size: 12px; padding: 5px 8px; text-align: left; align-self: flex-start; }
  .add:hover { border-color: color-mix(in srgb, var(--accent) 45%, var(--hairline)); color: var(--accent-strong, var(--accent)); }

  .danger-row { display: flex; align-items: center; gap: 14px; padding-top: 4px; }
  .remove { color: var(--danger); background: transparent; border: 1px solid color-mix(in srgb, var(--danger) 40%, var(--hairline)); border-radius: 6px; padding: 6px 14px; font-size: 12px; font-weight: 550; flex: 0 0 auto; }
  .remove:hover { background: color-mix(in srgb, var(--danger) 10%, transparent); }
  .remove:focus-visible, .del:focus-visible, .add:focus-visible, .iconbtn:focus-visible { outline: none; box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 30%, transparent); }
</style>
