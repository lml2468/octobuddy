<script lang="ts">
  // Octo 集成 pane: the bot's binding to its octo IM identity — API URL,
  // bf_ token, robot id — plus the octo-cli disk-profile status (and a
  // "重新登录" action to repair it without re-saving the whole config).
  //
  // Why disk-profile management lives here: when the agent calls octo-cli
  // with OCTO_BOT_ID env set (always set for XClaw bots), octo-cli does a
  // disk-profile lookup keyed by robot id and IGNORES OCTO_BOT_TOKEN entirely.
  // A missing profile fails the very first octo-cli call from the agent —
  // see configstore.Save's auto-Login + this pane's manual "重新登录" button
  // for the recovery path.
  import { XClawService } from "../../../bindings/github.com/lml2468/xclaw/desktop";
  import { errMsg } from "../errors";
  import type { BotConfig } from "../../../bindings/github.com/lml2468/xclaw/desktop/internal/configstore/models";

  let { bot = $bindable<BotConfig>(), botStatus, ondirty, isPreview = false }:
    { bot: BotConfig; botStatus: { connected: boolean; lastError?: string } | null; ondirty: () => void; isPreview?: boolean } = $props();

  let revealToken = $state(false);
  let editBotId = $state(false);

  // OCTO_BOT_ID lives in env, mirrored here for a typed edit + reactive write-back.
  let robotId = $state("");
  $effect(() => {
    bot.id; // re-seed on bot switch
    robotId = bot.env?.OCTO_BOT_ID ?? "";
    editBotId = false;
    refreshCliStatus();
  });
  function commitRobotId() {
    const env = { ...(bot.env ?? {}) };
    if (robotId.trim()) env.OCTO_BOT_ID = robotId.trim();
    else delete env.OCTO_BOT_ID;
    bot.env = env;
    ondirty();
  }

  // octo-cli profile status (preview-mode mock or live).
  let cliRegistered = $state(false);
  let cliRobotId = $state("");
  let cliBusy = $state(false);
  let cliError = $state("");
  let cliNotice = $state("");
  async function refreshCliStatus() {
    cliError = "";
    cliNotice = "";
    if (isPreview) {
      cliRegistered = bot.id === "main";
      cliRobotId = bot.env?.OCTO_BOT_ID ?? "";
      return;
    }
    try {
      const s = await XClawService.OctoCliStatus(bot.id);
      cliRegistered = !!s?.registered;
      cliRobotId = s?.robotId ?? "";
    } catch (e) { cliError = errMsg(e); }
  }
  async function relogin() {
    cliBusy = true; cliError = ""; cliNotice = "";
    try {
      if (isPreview) { cliRegistered = true; cliNotice = "已写入（preview mock）"; }
      else {
        await XClawService.OctoCliRelogin(bot.id);
        // The relogin path knows it just wrote the profile — flip state locally
        // instead of round-tripping refreshCliStatus (which would re-parse
        // config.json + re-read keychain to confirm what we already know).
        cliRegistered = true;
        cliRobotId = robotId;
        cliNotice = "已写入 octo-cli profile";
      }
    } catch (e) { cliError = errMsg(e); }
    finally { cliBusy = false; }
  }
  async function logout() {
    cliBusy = true; cliError = ""; cliNotice = "";
    try {
      if (isPreview) { cliRegistered = false; cliNotice = "已删除（preview mock）"; }
      else {
        await XClawService.OctoCliLogout(bot.id);
        cliRegistered = false;
        cliNotice = "已删除 octo-cli profile";
      }
    } catch (e) { cliError = errMsg(e); }
    finally { cliBusy = false; }
  }
</script>

<div class="pane">
  <fieldset>
    <legend>Octo 服务端</legend>
    <label>API URL <input bind:value={bot.apiUrl} oninput={ondirty} placeholder="https://im.example.com/api" /></label>
    <label>Bot Token (bf_…)
      <div class="tokenrow">
        <input type={revealToken ? "text" : "password"} bind:value={bot.octoToken} oninput={ondirty} placeholder="bf_…" autocomplete="off" />
        <button class="iconbtn" onclick={() => (revealToken = !revealToken)} type="button" aria-label={revealToken ? "隐藏" : "显示"}>
          {revealToken ? "隐藏" : "显示"}
        </button>
      </div>
      <small>存于系统钥匙串，绝不写入 config.json。</small>
    </label>
    <label>
      OCTO_BOT_ID（robot id）
      <div class="tokenrow">
        <input bind:value={robotId} oninput={commitRobotId} readonly={!editBotId} placeholder="例如 27abc1234567_bot" />
        {#if !editBotId}
          <button class="iconbtn" onclick={() => (editBotId = true)} type="button">修改</button>
        {:else}
          <button class="iconbtn" onclick={() => (editBotId = false)} type="button">完成</button>
        {/if}
      </div>
      <small>由「新增 Bot」向导从服务端拿回，手改可能与服务端实际 id 不一致 — 不要轻易动。</small>
    </label>
  </fieldset>

  {#if botStatus}
    <fieldset>
      <legend>连接状态</legend>
      <div class="status">
        <span class="dot" class:on={botStatus.connected}></span>
        <span class="label">{botStatus.connected ? "已连接" : "未连接"}</span>
        {#if !botStatus.connected && botStatus.lastError}
          <span class="err-detail">{botStatus.lastError}</span>
        {/if}
      </div>
    </fieldset>
  {/if}

  <fieldset>
    <legend>octo-cli 认证</legend>
    <p class="hint">
      Agent 调 octo-cli 走的是磁盘 profile（<code>~/.octo-cli/credentials.enc</code>）而不是环境变量；
      新建 / 修改 Bot Token 后需要把 profile 同步过去。「保存并重启」时会自动同步，遇到偏差也可以在这里手动重新登录。
    </p>
    <div class="cli-status">
      {#if cliRegistered}
        <span class="dot ok"></span>
        <span class="label">已注册</span>
        {#if cliRobotId}<code class="rid">{cliRobotId}</code>{/if}
      {:else}
        <span class="dot warn"></span>
        <span class="label">未注册</span>
        {#if cliRobotId}<code class="rid">期望 robot id: {cliRobotId}</code>{/if}
      {/if}
    </div>
    <div class="cli-actions">
      <button onclick={relogin} disabled={cliBusy || !bot.octoToken || !robotId}>重新登录</button>
      <button onclick={logout} disabled={cliBusy || !cliRegistered}>登出</button>
      <span class="spacer"></span>
      {#if cliError}<span class="err">⚠️ {cliError}</span>{:else if cliNotice}<span class="ok">✓ {cliNotice}</span>{/if}
    </div>
  </fieldset>
</div>

<style>
  .pane { display: flex; flex-direction: column; gap: 14px; }
  fieldset { border: 1px solid var(--hairline); border-radius: 12px; padding: 14px 16px; display: flex; flex-direction: column; gap: 12px; margin: 0; }
  legend { font-size: 11px; font-weight: 600; color: var(--ink-soft); text-transform: uppercase; letter-spacing: 0.04em; padding: 0 6px; }
  label { display: flex; flex-direction: column; gap: 5px; font-size: 12px; font-weight: 550; color: var(--ink-soft); }
  input { background: color-mix(in srgb, var(--ink) 4%, var(--surface)); border: 1px solid var(--hairline); border-radius: 10px; padding: 8px 11px; color: var(--ink); font-size: 13px; outline: none; transition: border-color .15s ease, box-shadow .15s ease; flex: 1; min-width: 0; }
  input:focus { border-color: color-mix(in srgb, var(--accent) 55%, var(--hairline)); box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 16%, transparent); }
  input[readonly] { background: color-mix(in srgb, var(--ink) 6%, transparent); color: var(--ink-soft); }
  small { color: var(--ink-faint); font-size: 11px; }

  .tokenrow { display: flex; gap: 6px; align-items: stretch; }
  .iconbtn { padding: 0 12px; border: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 4%, var(--surface)); color: var(--ink-soft); border-radius: 8px; font-size: 12px; }
  .iconbtn:hover { color: var(--ink); background: color-mix(in srgb, var(--ink) 8%, var(--surface)); }

  .status { display: flex; align-items: center; gap: 9px; font-size: 13px; }
  .dot { width: 9px; height: 9px; border-radius: 50%; background: var(--muted); flex: 0 0 auto; }
  .dot.on { background: var(--online, var(--success)); }
  .dot.ok { background: var(--online, var(--success)); }
  .dot.warn { background: var(--warning, #f0b429); }
  .err-detail { color: var(--danger); font-size: 12px; }
  .label { font-weight: 550; }

  .hint { margin: 0; font-size: 12px; color: var(--ink-faint); line-height: 1.6; }
  .hint code { font-family: var(--mono); font-size: 11.5px; padding: 1px 5px; background: color-mix(in srgb, var(--ink) 6%, transparent); border-radius: 4px; }
  .cli-status { display: flex; align-items: center; gap: 9px; font-size: 13px; }
  .cli-status .rid { font-family: var(--mono); font-size: 11.5px; color: var(--ink-soft); padding: 1px 6px; background: color-mix(in srgb, var(--ink) 6%, transparent); border-radius: 4px; }
  .cli-actions { display: flex; align-items: center; gap: 8px; }
  .cli-actions .spacer { flex: 1; }
  .cli-actions button { padding: 6px 14px; border-radius: 9px; border: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 4%, var(--surface)); color: var(--ink); font-size: 12px; font-weight: 550; }
  .cli-actions button:hover:not(:disabled) { background: color-mix(in srgb, var(--ink) 8%, var(--surface)); }
  .cli-actions button:disabled { opacity: .45; cursor: default; }
  .err { color: var(--danger); font-size: 12px; }
  .ok { color: #5aa873; font-size: 12px; }
  .cli-actions button:focus-visible, .iconbtn:focus-visible { outline: none; box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 30%, transparent); }
</style>
