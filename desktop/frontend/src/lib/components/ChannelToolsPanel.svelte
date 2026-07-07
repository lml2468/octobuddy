<script lang="ts">
 // Per-channel/DM tool panel: a popover over the chat header's tool icon that
 // scopes which tools the agent may use IN THIS conversation, writing through to
 // agent.tools.channels[sessionKey]. "跟随 Bot 默认" (unconfigured) falls back to
 // the bot-level default; "本会话自定义" lets the operator prune WITHIN the bot's
 // own selected tool set — a per-channel override can only narrow, never add
 // tools the bot itself doesn't have. So the candidate pool is the bot's
 // bot-level default (BotDefaultTools), NOT the global probed superset.
  import { OctoBuddyService } from "../../../bindings/github.com/lml2468/octobuddy/desktop";
  import { store } from "../store.svelte";

  let { botId, sessionKey, onclose }:
    { botId: string; sessionKey: string; onclose: () => void } = $props();

  const toolset = $derived(store.toolset);
  let configured = $state(false);     // false = follow bot default
  let scoped = $state<Set<string>>(new Set());
  let loaded = $state(false);
  let error = $state("");

  const mcpPattern = (s: string) => `mcp__${s}__*`;
  const isMCP = (t: string) => t.startsWith("mcp__");

  // The bot's bot-level default tool set — the candidate pool a per-channel
  // override narrows. When the bot is scoped (botScoped), candidates ARE that
  // list; when unscoped (driver default), candidates fall back to the full
  // probed built-ins + the bot's configured MCP servers.
  let botScoped = $state(false);
  let botDefault = $state<string[]>([]);   // the bot's tools.default when scoped
  let mcpServers = $state<string[]>([]);   // bot's .mcp.json server names

  // Built-in candidates: the bot's scoped built-ins, else the full probed set.
  const builtinCandidates = $derived(
    botScoped
      ? botDefault.filter((t) => !isMCP(t)).sort()
      : [...(toolset?.headlessSafe ?? [])].filter((t) => !isMCP(t)).sort(),
  );
  // MCP candidates (as mcp__<server>__* patterns): from the bot's scoped MCP
  // entries, else from the bot's configured servers.
  const mcpCandidates = $derived(
    botScoped
      ? botDefault.filter(isMCP).sort()
      : mcpServers.map(mcpPattern).sort(),
  );
  // Scoped channel names that aren't in the candidate pool — removable "超出 Bot
  // 集" chips (shouldn't normally happen now; kept so a legacy override that
  // included a wider set stays visible and removable rather than silently lost).
  const candidateSet = $derived(new Set([...builtinCandidates, ...mcpCandidates]));
  const extraScoped = $derived([...scoped].filter((t) => !candidateSet.has(t)).sort());

  async function load() {
    try {
      if (store.preview) {
        configured = false; scoped = new Set(); loaded = true; return;
      }
      const info = await OctoBuddyService.ChannelTools(botId, sessionKey);
      configured = !!info?.configured;
      scoped = new Set(info?.tools ?? []);
      // The bot's bot-level default (candidate pool).
      const bd = await OctoBuddyService.BotDefaultTools(botId);
      botScoped = !!bd?.scoped;
      botDefault = bd?.tools ?? [];
      // The bot's configured MCP servers (used only when the bot is unscoped).
      try {
        const raw = (await OctoBuddyService.LoadMCPConfig(botId)) ?? "";
        mcpServers = Object.keys(JSON.parse(raw || "{}")?.mcpServers ?? {}).sort();
      } catch {
        mcpServers = [];
      }
    } catch (e) {
      error = String(e);
    }
    loaded = true;
  }
  $effect(() => { if (!loaded) void load(); });

  // Serialize commits: every toggle/mode change writes the WHOLE current set
  // via SetChannelTools, so two fire-and-forget writes completing out of order
  // could persist a stale snapshot (lost update). Chain them so each write
  // starts only after the previous resolves, and each sends the latest state.
  //
  // Only the LATEST commit owns the error banner: a `seq` stamped at enqueue
  // time gates the outcome write, so an earlier failed write is not silently
  // cleared by a later success (and vice-versa) — the banner reflects the most
  // recently issued write, not whichever happened to finish last.
  let commitChain: Promise<void> = Promise.resolve();
  let commitSeq = 0;
  function commit() {
    const mine = ++commitSeq;
    commitChain = commitChain.then(async () => {
      if (store.preview) return;
      try {
        await OctoBuddyService.SetChannelTools(botId, sessionKey, configured, configured ? [...scoped] : []);
        if (mine === commitSeq) error = "";
      } catch (e) {
        if (mine === commitSeq) error = String(e);
      }
    });
  }

  function useCustom() {
    // Seed from the bot's OWN candidate pool (built-ins it has + its MCP tools),
    // so "本会话自定义" starts all-on within the bot's set and the operator prunes
    // from there — never offering tools the bot itself doesn't have. Seeding from
    // [] would muzzle the channel, so guard the empty pool.
    const pool = [...builtinCandidates, ...mcpCandidates];
    if (pool.length === 0) return;
    configured = true;
    scoped = new Set(pool);
    commit();
  }
  function followDefault() {
    configured = false;
    commit();
  }
  function toggle(name: string) {
    const next = new Set(scoped);
    if (next.has(name)) next.delete(name);
    else next.add(name);
    scoped = next;
    commit();
  }
</script>

<div class="overlay" role="presentation" onclick={onclose}></div>
<div class="panel" role="dialog" aria-label="本会话可用工具">
  <header>
    <span>本会话可用工具</span>
    <button class="x" onclick={onclose} aria-label="关闭">✕</button>
  </header>

  {#if !toolset || !toolset.probed || (toolset.headlessSafe ?? []).length === 0}
    <p class="hint">尚未探测到工具集。安装/升级 Agent 后将自动填充。</p>
  {:else}
    <div class="modeseg">
      <button type="button" class:active={!configured} onclick={followDefault}>跟随 Bot 默认</button>
      <button type="button" class:active={configured} onclick={useCustom}>本会话自定义</button>
    </div>
    {#if configured}
      <div class="group">
        <span class="grouplabel">内置工具</span>
        <div class="toolgrid">
          {#each builtinCandidates as name (name)}
            <label class="chk"><input type="checkbox" checked={scoped.has(name)} onchange={() => toggle(name)} /> {name}</label>
          {/each}
        </div>
      </div>
      <div class="group">
        <span class="grouplabel">MCP 工具</span>
        {#if mcpCandidates.length === 0}
          <p class="hint">该 Bot 未启用 MCP 工具。</p>
        {:else}
          <div class="toolgrid">
            {#each mcpCandidates as name (name)}
              <label class="chk"><input type="checkbox" checked={scoped.has(name)} onchange={() => toggle(name)} /> {name}</label>
            {/each}
          </div>
        {/if}
      </div>
      {#if extraScoped.length}
        <div class="group">
          <span class="grouplabel">超出 Bot 集（将移除）</span>
          <div class="toolgrid">
            {#each extraScoped as name (name)}
              <label class="chk extra"><input type="checkbox" checked onchange={() => toggle(name)} /> {name} <span class="tag">超集</span></label>
            {/each}
          </div>
        </div>
      {/if}
      <p class="hint">本会话只能在该 Bot 已选工具内进一步收窄（全部取消 = 本会话禁用工具）。</p>
    {:else}
      <p class="hint">本会话使用该 Bot 的默认工具集。</p>
    {/if}
  {/if}
  {#if error}<p class="hint err">{error}</p>{/if}
</div>

<style>
  .overlay { position: fixed; inset: 0; z-index: 40; }
  .panel {
    position: absolute; top: 44px; right: 12px; z-index: 41; width: 320px; max-height: 70vh; overflow: auto;
    background: var(--surface); border: 1px solid var(--hairline); border-radius: 12px;
    box-shadow: 0 12px 32px color-mix(in srgb, var(--ink) 22%, transparent); padding: 12px 14px;
    display: flex; flex-direction: column; gap: 10px;
  }
  header { display: flex; align-items: center; justify-content: space-between; font-size: 12px; font-weight: 600; color: var(--ink-soft); }
  .x { width: 24px; height: 24px; border-radius: 7px; border: 1px solid var(--hairline); background: transparent; color: var(--ink-soft); }
  .x:hover { background: color-mix(in srgb, var(--ink) 7%, transparent); }
  .modeseg { display: inline-flex; border: 1px solid var(--hairline); border-radius: 9px; overflow: hidden; align-self: flex-start; }
  .modeseg button { padding: 6px 12px; font-size: 12px; font-weight: 550; background: transparent; color: var(--ink-soft); border: none; border-right: 1px solid var(--hairline); }
  .modeseg button:last-child { border-right: none; }
  .modeseg button.active { background: color-mix(in srgb, var(--accent) 16%, transparent); color: var(--accent-strong, var(--accent)); }
  .toolgrid { display: grid; grid-template-columns: 1fr 1fr; gap: 5px 12px; }
  .chk { display: flex; align-items: center; gap: 7px; font-size: 12px; color: var(--ink); }
  .chk.extra { color: var(--ink-soft); }
  .chk .tag { font-size: 10px; color: var(--ink-faint); border: 1px solid var(--hairline); border-radius: 5px; padding: 0 4px; }
  .group { display: flex; flex-direction: column; gap: 6px; }
  .grouplabel { font-size: 10px; font-weight: 600; color: var(--ink-soft); text-transform: uppercase; letter-spacing: 0.04em; }
  .hint { color: var(--ink-faint); font-size: 11px; margin: 0; }
  .hint.err { color: var(--danger); }
</style>
