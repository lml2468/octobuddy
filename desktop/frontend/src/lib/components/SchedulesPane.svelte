<script lang="ts">
 // Per-bot scheduled task manager. CRUD over the control bus (cron.create /
 // cron.list / cron.update / cron.delete) with a target picker that covers
 // Console (the desktop's own chat session), DM (free-form peer uid + optional
 // recent-DM hint), and Group (dropdown populated by octo-cli group list via
 // the GroupsList Wails method).
 //
 // The cron Manager only arms when agent.cron is true; when it's false this
 // pane shows a banner with a one-click "启用并重启" that flips the flag via
 // the existing SaveConfig + RestartCore flow. Create is disabled while the
 // banner is up so the user doesn't fire-and-forget into a daemon that won't
 // schedule anything.
  import type { BotConfig } from "../../../bindings/github.com/lml2468/xclaw/desktop/internal/configstore/models";
  import { XClawService } from "../../../bindings/github.com/lml2468/xclaw/desktop";
  import { store, CONSOLE_UID } from "../store.svelte";
  import { confirm } from "../confirm.svelte";
  import { errMsg } from "../errors";
  import { modal } from "../actions/modal";

  let { bot = $bindable<BotConfig>(), ondirty, isPreview = false }:
    { bot: BotConfig; ondirty: () => void; isPreview?: boolean } = $props();

 // Reactive view of this bot's tasks. cron.list response folds in via the
 // store's envelope handler, so any concurrent refresh from another component
 // (or a future tick-driven auto-poll) reaches us live.
  const tasks = $derived(store.schedules[bot.id] ?? []);

 // ---- on mount: ask the daemon for the current task list ----
  let listError = $state("");
 // armedCron snapshots the value the DAEMON is running with right now —
 // distinct from bot.cron, which is the editor's dirty value that flips
 // synchronously when the user clicks 启用 (but doesn't take effect until
 // SaveConfig + RestartCore complete). The CronList fetch is gated on the
 // snapshot so flipping bot.cron locally doesn't immediately fire a
 // CronList that the daemon would reject with "cron is not enabled" — the
 // race that lit up a red error banner the instant the user enabled the
 // feature. The snapshot is read once at mount and re-read when bot.id
 // changes (SettingsModal remounts the pane on bot switch).
  let armedCron = $state(bot.cron);
  async function refreshList() {
    if (isPreview || !armedCron) return;
    try {
      await XClawService.CronList(bot.id);
      listError = "";
    } catch (e) {
      listError = errMsg(e);
    }
  }
  $effect(() => {
   // Re-fetch only when the bot id changes — bot.cron edits are deliberately
   // NOT tracked here (see armedCron note above). When the operator clicks
   // 保存并重启 and the daemon comes back, SettingsModal closes + reopens,
   // which remounts this pane and re-reads armedCron from the freshly
   // loaded bot.cron — picking up the new effective value cleanly.
    bot.id;
    armedCron = bot.cron;
    refreshList();
  });

 // ---- groups dropdown (for the Group target picker) ----
  let groups = $state<{ id: string; name: string }[]>([]);
  let groupsError = $state("");
  async function loadGroups() {
    if (isPreview) {
      groups = [{ id: "grp-demo-1", name: "演示群A" }, { id: "grp-demo-2", name: "演示群B" }];
      return;
    }
    try {
      groups = (await XClawService.GroupsList(bot.id)) ?? [];
      groupsError = "";
    } catch (e) {
      groups = [];
      groupsError = errMsg(e);
    }
  }

 // ---- enable cron banner ----
 // The flow is two-step by design: clicking 启用 flips bot.cron locally +
 // marks the settings dirty, surfacing 保存并重启 in the SettingsModal
 // footer. Only after that round-trip does armedCron flip on the next
 // pane mount — at which point refreshList actually runs. Trying to
 // refresh immediately would surface the daemon's "cron not enabled"
 // error the instant the user clicks 启用 (the create-then-restart race).
  async function enableCron() {
    bot.cron = true;
    ondirty();
  }

 // ---- per-row enable/disable toggle ----
  async function toggleEnabled(taskId: string, enabled: boolean) {
    if (isPreview) return;
    try {
      await XClawService.CronUpdate({ botId: bot.id, id: taskId, enabled } as any);
      await refreshList();
    } catch (e) {
      listError = errMsg(e);
    }
  }

  async function deleteTask(taskId: string) {
    if (isPreview) return;
    if (!(await confirm({ message: "确认删除此定时任务？", confirmLabel: "删除", danger: true }))) return;
    try {
      await XClawService.CronDelete(bot.id, "", taskId);
      await refreshList();
    } catch (e) {
      listError = errMsg(e);
    }
  }

 // ---- create / edit modal ----
  type Target = "console" | "dm" | "group";
  let modalOpen = $state(false);
  let editingId = $state<string | null>(null);
  let formSchedule = $state("0 9 * * *");
  let formRecurring = $state(true);
  let formPrompt = $state("");
  let formTarget = $state<Target>("console");
  let formDmUid = $state("");
  let formGroupId = $state("");
  let formFromName = $state("");
  let formError = $state("");
  let formBusy = $state(false);

  function openCreate() {
    editingId = null;
    formSchedule = "0 9 * * *";
    formRecurring = true;
    formPrompt = "";
    formTarget = "console";
    formDmUid = "";
    formGroupId = "";
    formFromName = "";
    formError = "";
    modalOpen = true;
    loadGroups();
  }

  function openEdit(task: any) {
    editingId = task.id;
    formSchedule = task.schedule;
    formRecurring = task.recurring;
    formPrompt = task.prompt ?? "";
    formFromName = task.fromName ?? "";
    if (task.channelType === 3) {
      formTarget = "console";
    } else if (task.channelType === 2) {
      formTarget = "group";
      formGroupId = task.channelId ?? "";
    } else {
     // DM — the body doesn't echo back fromUid (operator-internal), so
     // the renderer can't pre-fill the peer uid on edit. Show the field
     // empty with a placeholder reminding the operator to re-enter if
     // changing the DM target; leaving it blank keeps the existing
     // binding (the daemon's Update accepts an empty FromUID since the
     // owner is server-resolved).
      formTarget = "dm";
      formDmUid = "";
    }
    formError = "";
    modalOpen = true;
    loadGroups();
  }

  async function submit() {
    formError = "";
    if (isPreview) { modalOpen = false; return; }
   // Channel coords derived from the target choice. ChannelType convention:
   //   1 = DM, 2 = Group, 3 = Console (matches core/cron/store.go).
   //
   // fromUid carries the TARGET of the task, distinct from auth uid (which
   // the server resolves itself). For DM the user-typed peer uid lands here;
   // for Console the backend stamps cron.ConsoleUID regardless so the GUI
   // sends CONSOLE_UID for clarity; for Group the backend ignores fromUid
   // and stamps the owner. On EDIT, an empty fromUid is the "preserve
   // existing target" signal — Manager.Update reads the stored task and
   // leaves coords alone when the body's coord triplet is zero.
    let channelId = "";
    let channelType = 1;
    let fromUid = "";
    let fromName = formFromName.trim();
    if (formTarget === "console") {
      channelType = 3;
      fromUid = CONSOLE_UID;
      if (!fromName) fromName = "控制台";
    } else if (formTarget === "group") {
      channelType = 2;
      channelId = formGroupId.trim();
      if (!channelId) { formError = "请选择一个群"; return; }
      // fromUid stays "" — Group tasks fire as the owner; backend ignores body fromUid for Group.
    } else {
      channelType = 1;
      fromUid = formDmUid.trim();
      if (!editingId && !fromUid) { formError = "请填写对方的 uid"; return; }
      // editingId + blank fromUid: preserve existing peer binding.
    }
    formBusy = true;
    try {
      if (editingId) {
        await XClawService.CronUpdate({
          botId: bot.id,
          id: editingId,
          schedule: formSchedule.trim(),
          prompt: formPrompt,
          recurring: formRecurring,
          channelId,
          channelType,
          fromUid,
          fromName,
        } as any);
      } else {
        await XClawService.CronCreate({
          botId: bot.id,
          schedule: formSchedule.trim(),
          prompt: formPrompt,
          recurring: formRecurring,
          channelId,
          channelType,
          fromUid,
          fromName,
        } as any);
      }
      modalOpen = false;
      await refreshList();
    } catch (e) {
      formError = errMsg(e);
    } finally {
      formBusy = false;
    }
  }

 // ---- formatters ----
  function targetLabel(task: any): string {
    if (task.channelType === 3) return "控制台";
    if (task.channelType === 2) {
      const g = groups.find((x) => x.id === task.channelId);
      return g ? `群 · ${g.name}` : `群 · ${task.channelId || "?"}`;
    }
    return `DM`;
  }
  function relTime(rfc3339: string | undefined): string {
    if (!rfc3339) return "—";
    const t = Date.parse(rfc3339);
    if (Number.isNaN(t)) return rfc3339;
    const diff = (t - Date.now()) / 1000;
    const abs = Math.abs(diff);
    let label: string;
    if (abs < 60) label = `${Math.round(abs)} 秒`;
    else if (abs < 3600) label = `${Math.round(abs / 60)} 分`;
    else if (abs < 86400) label = `${Math.round(abs / 3600)} 小时`;
    else label = `${Math.round(abs / 86400)} 天`;
    return diff >= 0 ? `${label}后` : `${label}前`;
  }
  function truncPrompt(s: string): string {
    if (!s) return "";
    return s.length > 60 ? s.slice(0, 60) + "…" : s;
  }
</script>

<div class="pane">
 <!--
   Two-stage banner: shown if cron isn't already armed in the daemon OR
   if the operator just flipped it on locally (pending restart). The
   second sub-banner reminds the operator that the actual scheduler arms
   only after 保存并重启 — otherwise they create tasks that vanish on the
   next config reload.
 -->
  {#if !armedCron && !bot.cron}
    <div class="banner">
      <div class="b-text">
        <strong>定时任务未启用</strong>
        <p>启用后将为该 Bot 加载 <code>~/.xclaw/{bot.id}/cron.json</code> 并定时触发。</p>
      </div>
      <button class="b-btn" onclick={enableCron}>启用</button>
    </div>
    <p class="hint">勾选后请到工具栏点 <strong>保存并重启</strong>，定时调度器才会真正起来。</p>
  {:else if !armedCron && bot.cron}
    <div class="banner pending">
      <div class="b-text">
        <strong>已启用 — 待重启生效</strong>
        <p>请点击工具栏的 <strong>保存并重启</strong>。重启后才能创建定时任务。</p>
      </div>
    </div>
  {/if}

  {#if listError}
    <div class="error">{listError}</div>
  {/if}

  <div class="head">
    <h3>共 {tasks.length} 条</h3>
    <button class="primary" onclick={openCreate} disabled={!armedCron}>+ 新增定时任务</button>
  </div>

  {#if tasks.length === 0}
    <div class="empty">还没有定时任务{armedCron ? "" : "（启用后再添加）"}。</div>
  {:else}
    <table class="grid">
      <thead><tr>
        <th class="c-en">启用</th>
        <th class="c-sch">Schedule</th>
        <th class="c-tgt">目标</th>
        <th>Prompt</th>
        <th class="c-when">下次</th>
        <th class="c-when">上次</th>
        <th class="c-act"></th>
      </tr></thead>
      <tbody>
        {#each tasks as task (task.id)}
          <tr>
            <td>
              <label class="sw" title={task.enabled ? "已启用" : "已禁用"}>
                <input type="checkbox" checked={task.enabled} onchange={(e) => toggleEnabled(task.id, (e.target as HTMLInputElement).checked)} />
                <span class="slider"></span>
              </label>
            </td>
            <td class="mono">{task.schedule}{task.recurring ? "" : " (一次)"}</td>
            <td>{targetLabel(task)}</td>
            <td class="prompt" title={task.prompt}>{truncPrompt(task.prompt)}</td>
            <td class="mono">{relTime(task.nextRun)}</td>
            <td class="mono">{task.lastRun ? relTime(task.lastRun) : "—"}</td>
            <td class="acts">
              <button class="iconbtn" onclick={() => openEdit(task)} title="编辑">✎</button>
              <button class="iconbtn danger" onclick={() => deleteTask(task.id)} title="删除">✕</button>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>

{#if modalOpen}
 <!-- svelte-ignore a11y_click_events_have_key_events -->
  <div class="scrim" onclick={() => !formBusy && (modalOpen = false)} role="presentation">
    <div class="modal" use:modal={{ onclose: () => !formBusy && (modalOpen = false) }} onclick={(e) => e.stopPropagation()} role="dialog" aria-label={editingId ? "编辑定时任务" : "新增定时任务"} tabindex="-1">
      <header><h3>{editingId ? "编辑定时任务" : "新增定时任务"}</h3><button class="x" onclick={() => !formBusy && (modalOpen = false)} aria-label="关闭">✕</button></header>
      <div class="body">
        <label>
          Schedule <span class="hint">5 字段 cron 表达式（如 <code>0 9 * * 1-5</code>）或一次性 ISO 时间（如 <code>2026-07-01T09:00:00+08:00</code>）</span>
          <input class="mono" bind:value={formSchedule} placeholder="0 9 * * *" />
        </label>
        <label class="check">
          <input type="checkbox" bind:checked={formRecurring} />
          重复触发（关闭则触发一次后自动删除）
        </label>
        <label>
          Prompt <span class="hint">≤ 2048 字符。任务到点时作为入站消息送进 agent</span>
          <textarea bind:value={formPrompt} rows="4" placeholder="例如：早安。汇总下昨天的会议纪要"></textarea>
        </label>
        <div class="targetbox">
          <span class="lbl">目标</span>
          <div class="seg">
            <button class:active={formTarget === "console"} onclick={() => formTarget = "console"} type="button">控制台</button>
            <button class:active={formTarget === "dm"} onclick={() => formTarget = "dm"} type="button">DM</button>
            <button class:active={formTarget === "group"} onclick={() => formTarget = "group"} type="button">群</button>
          </div>
          {#if formTarget === "console"}
            <small class="hint">触发结果会出现在该 Bot 的桌面 Console 会话。</small>
          {:else if formTarget === "dm"}
            <input class="mono" bind:value={formDmUid} placeholder={editingId ? "留空保持当前绑定" : "对方 uid"} />
            <small class="hint">直接到点向该 uid 的 DM 发起一次回合。</small>
          {:else}
            <select bind:value={formGroupId}>
              <option value="" disabled>— 选择一个群 —</option>
              {#each groups as g}
                <option value={g.id}>{g.name}</option>
              {/each}
            </select>
            {#if groupsError}
              <small class="err">无法加载群列表：{groupsError}</small>
            {:else if groups.length === 0}
              <small class="hint">没有群，或该 Bot 的 octo-cli 未登录。</small>
            {/if}
          {/if}
        </div>
        <label>
          From Name <span class="hint">可选。任务触发时作为调用者显示名</span>
          <input bind:value={formFromName} placeholder="（可空）" />
        </label>
        {#if formError}<div class="error">{formError}</div>{/if}
      </div>
      <footer>
        <button class="ghost" onclick={() => !formBusy && (modalOpen = false)} disabled={formBusy}>取消</button>
        <button class="primary" onclick={submit} disabled={formBusy}>{formBusy ? "保存中…" : (editingId ? "保存" : "创建")}</button>
      </footer>
    </div>
  </div>
{/if}

<style>
  .pane { display: flex; flex-direction: column; gap: 14px; }
  .banner { display: flex; align-items: center; gap: 14px; padding: 12px 14px; border-radius: 10px; border: 1px solid color-mix(in srgb, var(--warn, #c98a07) 35%, var(--hairline)); background: color-mix(in srgb, var(--warn, #c98a07) 8%, transparent); }
  .banner.pending { border-color: color-mix(in srgb, var(--accent) 35%, var(--hairline)); background: color-mix(in srgb, var(--accent) 8%, transparent); }
  .b-text { flex: 1; }
  .b-text strong { font-size: 13px; }
  .b-text p { margin: 4px 0 0; font-size: 12px; color: var(--ink-soft); }
  .b-text code { font-family: var(--mono); font-size: 11px; padding: 1px 5px; background: color-mix(in srgb, var(--ink) 6%, transparent); border-radius: 4px; }
  .b-btn { padding: 6px 14px; border-radius: 7px; border: 1px solid var(--hairline); background: var(--surface); color: var(--ink); font-size: 12px; font-weight: 550; }
  .b-btn:hover:not(:disabled) { background: color-mix(in srgb, var(--accent) 10%, var(--surface)); border-color: color-mix(in srgb, var(--accent) 35%, var(--hairline)); }

  .hint { font-size: 11px; color: var(--ink-faint); }
  .head { display: flex; align-items: center; justify-content: space-between; }
  .head h3 { margin: 0; font-size: 13px; color: var(--ink-soft); font-weight: 600; }
  .empty { color: var(--ink-faint); font-size: 12px; padding: 24px 0; text-align: center; }
  .error { padding: 8px 12px; border-radius: 6px; background: color-mix(in srgb, var(--danger) 8%, transparent); color: var(--danger); font-size: 12px; }

  .grid { width: 100%; border-collapse: collapse; font-size: 12px; }
  .grid th { text-align: left; font-weight: 550; color: var(--ink-soft); padding: 6px 8px; border-bottom: 1px solid var(--hairline); font-size: 11px; text-transform: uppercase; letter-spacing: 0.04em; }
  .grid td { padding: 8px; border-bottom: 1px solid var(--hairline); vertical-align: middle; }
  .grid tr:hover td { background: color-mix(in srgb, var(--ink) 3%, transparent); }
  .grid .c-en { width: 50px; }
  .grid .c-sch { width: 130px; }
  .grid .c-tgt { width: 110px; }
  .grid .c-when { width: 100px; color: var(--ink-soft); }
  .grid .c-act { width: 70px; text-align: right; }
  .grid td.mono { font-family: var(--mono); }
  .grid td.prompt { color: var(--ink-soft); overflow: hidden; text-overflow: ellipsis; max-width: 200px; }
  .acts { display: flex; gap: 4px; justify-content: flex-end; }

  .iconbtn { width: 26px; height: 26px; border-radius: 6px; border: 1px solid var(--hairline); background: var(--surface); color: var(--ink-soft); display: grid; place-items: center; transition: background .14s ease; }
  .iconbtn:hover { background: color-mix(in srgb, var(--accent) 10%, var(--surface)); color: var(--accent); }
  .iconbtn.danger:hover { background: color-mix(in srgb, var(--danger) 10%, var(--surface)); color: var(--danger); }

 /* iOS-style toggle */
  .sw { display: inline-block; position: relative; width: 36px; height: 20px; }
  .sw input { opacity: 0; width: 0; height: 0; }
  .sw .slider { position: absolute; inset: 0; background: color-mix(in srgb, var(--ink) 20%, transparent); border-radius: 999px; transition: background .14s ease; }
  .sw .slider::before { content: ""; position: absolute; top: 2px; left: 2px; width: 16px; height: 16px; background: #fff; border-radius: 50%; transition: transform .14s ease; }
  .sw input:checked + .slider { background: var(--accent); }
  .sw input:checked + .slider::before { transform: translateX(16px); }

  .primary { padding: 7px 14px; border-radius: 7px; border: none; background: var(--accent); color: #fff; font-size: 12px; font-weight: 550; }
  .primary:hover:not(:disabled) { background: color-mix(in srgb, var(--accent) 90%, black); }
  .primary:disabled { opacity: 0.5; cursor: not-allowed; }
  .ghost { padding: 7px 14px; border-radius: 7px; border: 1px solid var(--hairline); background: transparent; color: var(--ink); font-size: 12px; }

 /* modal */
  .scrim { position: fixed; inset: 0; background: rgba(0,0,0,0.35); display: grid; place-items: center; z-index: 100; }
  .modal { width: 520px; max-width: 92vw; max-height: 86vh; display: flex; flex-direction: column; background: var(--surface); border: 1px solid var(--hairline); border-radius: 12px; box-shadow: 0 20px 60px rgba(0,0,0,0.25); overflow: hidden; }
  .modal header { display: flex; align-items: center; padding: 12px 16px; border-bottom: 1px solid var(--hairline); }
  .modal header h3 { margin: 0; flex: 1; font-size: 13px; font-weight: 600; }
  .modal header .x { width: 26px; height: 26px; border-radius: 6px; border: none; background: transparent; color: var(--ink-soft); }
  .modal header .x:hover { background: color-mix(in srgb, var(--ink) 8%, transparent); }
  .modal .body { padding: 14px 16px; overflow-y: auto; display: flex; flex-direction: column; gap: 12px; }
  .modal label { display: flex; flex-direction: column; gap: 4px; font-size: 12px; font-weight: 550; color: var(--ink-soft); }
  .modal label.check { flex-direction: row; align-items: center; gap: 6px; font-weight: 400; color: var(--ink); }
  .modal input, .modal textarea, .modal select { background: color-mix(in srgb, var(--ink) 4%, var(--surface)); border: 1px solid var(--hairline); border-radius: 8px; padding: 7px 10px; color: var(--ink); font-size: 12px; outline: none; transition: border-color .15s ease; }
  .modal input:focus, .modal textarea:focus, .modal select:focus { border-color: color-mix(in srgb, var(--accent) 55%, var(--hairline)); }
  .modal .mono, .modal input.mono { font-family: var(--mono); }
  .modal textarea { resize: vertical; font-family: var(--ui); }
  .modal footer { display: flex; gap: 8px; justify-content: flex-end; padding: 12px 16px; border-top: 1px solid var(--hairline); }

  .targetbox { display: flex; flex-direction: column; gap: 6px; }
  .targetbox .lbl { font-size: 12px; font-weight: 550; color: var(--ink-soft); }
  .seg { display: flex; border: 1px solid var(--hairline); border-radius: 8px; overflow: hidden; align-self: flex-start; }
  .seg button { padding: 6px 12px; border: none; background: transparent; color: var(--ink-soft); font-size: 12px; border-right: 1px solid var(--hairline); }
  .seg button:last-child { border-right: none; }
  .seg button.active { background: var(--accent); color: #fff; }
  .seg button:hover:not(.active) { background: color-mix(in srgb, var(--ink) 6%, transparent); }
  .err { color: var(--danger); font-size: 11px; }
</style>
