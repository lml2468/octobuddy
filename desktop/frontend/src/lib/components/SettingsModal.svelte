<script lang="ts">
  // Unified per-bot settings modal: a left bot list (+ 新增 Bot via wizard) and
  // a right pane that switches through 4 tabs — 基础信息 / Octo 集成 / 技能 /
  // 工作流. Replaces the prior 4 sibling modals (ConfigEditor / SkillsPanel /
  // WorkflowsPanel / TokenUsage) and the SettingsHeader segmented nav that
  // routed between them.
  //
  // State ownership: this modal owns the editable `bots[]` array; BasicInfoPane
  // and OctoIntegrationPane mutate the selected entry and flip a single dirty
  // flag, surfaced in the footer's 保存/保存并重启 (the same SaveConfig path the
  // old ConfigEditor used). SkillsPane and WorkflowsPane write through to disk
  // immediately and don't participate in the dirty flag.
  import { XClawService } from "../../../bindings/github.com/lml2468/xclaw/desktop";
  import { BotConfig } from "../../../bindings/github.com/lml2468/xclaw/desktop/internal/configstore/models";
  import { store } from "../store.svelte";
  import { modal } from "../actions/modal";
  import { confirm } from "../confirm.svelte";
  import { errMsg } from "../errors";
  import Avatar from "./Avatar.svelte";
  import BasicInfoPane from "./BasicInfoPane.svelte";
  import OctoIntegrationPane from "./OctoIntegrationPane.svelte";
  import SkillsPane from "./SkillsPane.svelte";
  import WorkflowsPane from "./WorkflowsPane.svelte";

  type TabKey = "basic" | "octo" | "skills" | "workflows";
  let { onclose, initialTab = "basic" as TabKey }:
    { onclose: () => void; initialTab?: TabKey } = $props();

  const isPreview = new URLSearchParams(location.search).has("preview");

  let bots = $state<BotConfig[]>([]);
  let sel = $state(0);
  // svelte-ignore state_referenced_locally — `initialTab` is the one-shot seed; later changes to the prop are intentionally ignored (it's the URL/event-passed default, not a live binding).
  let activeTab = $state<TabKey>(initialTab);
  let dirty = $state(false);
  let busy = $state(false);
  let saved = $state(false);
  let error = $state("");
  let loadedIds: string[] = [];

  const current = $derived(bots[sel] ?? null);

  // --- Add-bot wizard (uk → bf, falling back to a blank manual entry) ---
  let wizardOpen = $state(false);
  let wizId = $state("");
  let wizApiUrl = $state("");
  let wizApiKey = $state("");
  let wizName = $state("");
  let wizBusy = $state(false);
  let wizError = $state("");

  load();

  async function load() {
    if (isPreview) {
      bots = [
        new BotConfig({ id: "main", apiUrl: "https://im.example.com/api", model: "claude-opus-4-8", gatewayBaseUrl: "https://gw.example/v1", env: { OCTO_BOT_ID: "27abc1234567" }, soul: "You are Atlas, the team's ops copilot.", agents: "Confirm before destructive actions." }),
        new BotConfig({ id: "research", apiUrl: "https://im.example.com/api", env: { OCTO_BOT_ID: "27xyz9876543" } }),
      ];
      sel = 0;
      return;
    }
    try {
      bots = (await XClawService.LoadConfig()) ?? [];
      loadedIds = bots.map((b) => b.id);
      sel = 0;
    } catch (e) {
      error = errMsg(e);
    }
  }

  // Per-tab dirty hookup: the panes flip the shared flag whenever an editable
  // field changes. Skills/Workflows panes don't call this — they write through.
  function markDirty() { dirty = true; saved = false; }

  // --- left list interactions ---
  async function selectBot(i: number) {
    if (i === sel) return;
    if (dirty && !(await confirm({ message: "有未保存的改动,确认切换 Bot?", confirmLabel: "切换", danger: true }))) return;
    sel = i;
    dirty = false;
  }
  // Switching tabs within the same bot keeps unsaved edits — both basic and
  // octo edit the same bot[sel] entry, so a save still captures everything.

  // --- save ---
  async function save(restart: boolean) {
    if (!current) return;
    error = ""; saved = false; busy = true;
    try {
      const present = new Set(bots.map((b) => b.id));
      const removed = loadedIds.filter((id) => !present.has(id));
      await XClawService.SaveConfig(bots, removed);
      loadedIds = bots.map((b) => b.id);
      saved = true;
      dirty = false;
      if (restart) { await XClawService.RestartCore(); store.bots = []; XClawService.BotsList(); onclose(); }
    } catch (e) {
      error = errMsg(e);
    } finally {
      busy = false;
    }
  }

  // --- close guard ---
  async function leave() {
    if (dirty && !(await confirm({ message: "有未保存的改动,确认离开?", confirmLabel: "离开" }))) return;
    onclose();
  }

  // --- add bot ---
  function openWizard() {
    let n = 1;
    const taken = new Set(bots.map((b) => b.id));
    while (taken.has(`bot${n}`)) n++;
    wizId = `bot${n}`;
    wizApiUrl = bots[0]?.apiUrl && bots[0].apiUrl !== "https://" ? bots[0].apiUrl : "";
    wizApiKey = ""; wizName = ""; wizError = "";
    wizardOpen = true;
  }
  function manualAdd() {
    wizardOpen = false;
    const b = new BotConfig({ id: wizId, apiUrl: bots[0]?.apiUrl ?? "https://" });
    bots = [...bots, b];
    sel = bots.length - 1;
    activeTab = "basic";
    dirty = true;
  }
  async function createBot() {
    const id = wizId.trim();
    if (!id) { wizError = "请填写 Bot ID"; return; }
    if (bots.some((b) => b.id === id)) { wizError = `Bot ID 「${id}」已存在`; return; }
    wizError = ""; wizBusy = true;
    try {
      const r = await XClawService.OctoAddBot(wizApiUrl.trim(), wizApiKey.trim(), wizName.trim());
      const b = new BotConfig({ id, apiUrl: wizApiUrl.trim(), octoToken: r.botToken, env: { OCTO_BOT_ID: r.robotId } });
      bots = [...bots, b];
      sel = bots.length - 1;
      activeTab = "octo";
      dirty = true;
      wizardOpen = false;
    } catch (e) {
      wizError = errMsg(e);
    } finally {
      wizBusy = false;
    }
  }

  // --- delete bot (from BasicInfoPane via callback) ---
  async function deleteBot() {
    if (!current) return;
    const id = current.id || "(未命名)";
    if (!(await confirm({ message: `删除 Bot 「${id}」?此操作会从下次保存时移除其配置。`, confirmLabel: "删除", danger: true }))) return;
    bots = bots.filter((_, idx) => idx !== sel);
    sel = Math.max(0, Math.min(sel, bots.length - 1));
    dirty = true;
  }

  const TABS: { key: TabKey; label: string }[] = [
    { key: "basic",     label: "基础信息" },
    { key: "octo",      label: "Octo 集成" },
    { key: "skills",    label: "技能" },
    { key: "workflows", label: "工作流" },
  ];

  // Arrow-key tab navigation per WAI-ARIA APG tablist pattern (←/→ cycle
  // visible tabs; Home/End jump to the ends). Pairs with the roving
  // tabindex above (only the active tab is in the tab order; others are -1)
  // so screen-reader keyboard users don't fall through 4 buttons with Tab.
  function onTabKey(e: KeyboardEvent) {
    const cur = TABS.findIndex((t) => t.key === activeTab);
    if (cur < 0) return;
    let next = cur;
    if (e.key === "ArrowRight") next = (cur + 1) % TABS.length;
    else if (e.key === "ArrowLeft") next = (cur - 1 + TABS.length) % TABS.length;
    else if (e.key === "Home") next = 0;
    else if (e.key === "End") next = TABS.length - 1;
    else return;
    e.preventDefault();
    activeTab = TABS[next].key;
    // Move focus to the newly-selected tab so the user can keep arrowing.
    queueMicrotask(() => {
      const btns = (e.currentTarget as HTMLElement | null)?.querySelectorAll<HTMLButtonElement>('button[role="tab"]');
      btns?.[next]?.focus();
    });
  }
</script>

<div class="scrim" onclick={leave} role="presentation">
  <!-- svelte-ignore a11y_click_events_have_key_events (use:modal handles Escape/Tab) -->
  <div class="modal" use:modal={{ onclose: leave }} onclick={(e) => e.stopPropagation()} role="dialog" aria-label="设置" tabindex="-1">
    <header>
      <h2>设置</h2>
      <span class="hspacer"></span>
      <button class="x" onclick={leave} aria-label="关闭">✕</button>
    </header>

    <div class="body">
      <aside class="rail">
        {#each bots as b, i (b)}
          <button class="botrow" class:sel={i === sel} onclick={() => selectBot(i)}>
            <Avatar name={b.id || "bot"} size={26} />
            <span class="bn">{b.id || "(未命名)"}</span>
            <span class="bdot" class:on={store.bots.find((x) => x.id === b.id)?.connected}></span>
          </button>
        {/each}
        <button class="add" onclick={openWizard}>+ 新增 Bot</button>
      </aside>

      <section class="pane">
        {#if !current}
          <div class="empty">
            <p>还没有 Bot。点左边的 <strong>+ 新增 Bot</strong> 开始。</p>
          </div>
        {:else}
          <div class="seg" role="tablist" tabindex="-1" aria-label="设置分区" onkeydown={onTabKey}>
            {#each TABS as t (t.key)}
              <button role="tab" aria-selected={t.key === activeTab} tabindex={t.key === activeTab ? 0 : -1} class:on={t.key === activeTab} onclick={() => (activeTab = t.key)}>{t.label}</button>
            {/each}
          </div>

          <div class="content">
            {#if activeTab === "basic"}
              <BasicInfoPane bind:bot={bots[sel]} ondirty={markDirty} ondelete={deleteBot} />
            {:else if activeTab === "octo"}
              <OctoIntegrationPane bind:bot={bots[sel]} botStatus={store.bots.find((x) => x.id === current.id) ?? null} ondirty={markDirty} {isPreview} />
            {:else if activeTab === "skills"}
              <SkillsPane botId={current.id} {isPreview} />
            {:else}
              <WorkflowsPane botId={current.id} {isPreview} />
            {/if}
          </div>
        {/if}
      </section>
    </div>

    {#if current}
      <footer>
        {#if error}<span class="err">⚠️ {error}</span>{:else if saved}<span class="ok">✓ 已保存</span>{/if}
        <span class="spacer"></span>
        <button onclick={() => save(false)} disabled={busy || !dirty}>保存</button>
        <button class="primary" onclick={() => save(true)} disabled={busy || !dirty}>保存并重启</button>
      </footer>
    {/if}

    {#if wizardOpen}
      <!-- svelte-ignore a11y_click_events_have_key_events -->
      <div class="wizscrim" onclick={() => !wizBusy && (wizardOpen = false)} role="presentation">
        <div class="wizcard" use:modal={{ onclose: () => !wizBusy && (wizardOpen = false) }} onclick={(e) => e.stopPropagation()} role="dialog" aria-label="新增 Bot" tabindex="-1">
          <div class="wizhead">
            <h3>新增 Bot</h3>
            <button class="x" onclick={() => !wizBusy && (wizardOpen = false)} aria-label="关闭">✕</button>
          </div>
          <p class="wizsub">用你的 octo API Key（uk_…）在服务器创建 Bot，自动获取并保存 Bot Token。</p>
          <div class="wizform">
            <label>Bot ID
              <input bind:value={wizId} placeholder="my-bot" />
              <small>本地标识（目录、会话），与 Octo 上的 Bot ID 无关。</small>
            </label>
            <label>API URL <input bind:value={wizApiUrl} placeholder="https://im.example.com/api" /></label>
            <label>API Key
              <input type="password" bind:value={wizApiKey} placeholder="uk_…" autocomplete="off" />
              <small>仅用于本次创建，不会被保存。</small>
            </label>
            <label>Bot 名称 <input bind:value={wizName} placeholder="My Bot" /></label>
          </div>
          {#if wizError}<div class="wizerr">⚠️ {wizError}</div>{/if}
          <div class="wizbtns">
            <button class="link" onclick={manualAdd} disabled={wizBusy}>手动添加（已有 Token）</button>
            <span class="spacer"></span>
            <button onclick={() => (wizardOpen = false)} disabled={wizBusy}>取消</button>
            <button class="primary" onclick={createBot} disabled={wizBusy}>{wizBusy ? "创建中…" : "创建"}</button>
          </div>
        </div>
      </div>
    {/if}
  </div>
</div>

<style>
  .scrim { position: fixed; inset: 0; z-index: 50; background: var(--window-grad); display: block; }
  .modal { width: 100%; height: 100%; position: relative; display: flex; flex-direction: column; background: var(--glass); backdrop-filter: blur(24px) saturate(180%); -webkit-backdrop-filter: blur(24px) saturate(180%); border: none; border-radius: 0; box-shadow: none; overflow: hidden; color: var(--ink); font-family: var(--ui); }

  header { display: flex; align-items: center; gap: 12px; height: var(--header-h); padding: 0 18px 0 92px; -webkit-app-region: drag; border-bottom: 1px solid var(--border-soft, var(--hairline)); }
  header h2 { font-size: 17px; font-weight: 600; margin: 0; }
  header .hspacer { flex: 1; }
  header .x { -webkit-app-region: no-drag; width: 30px; height: 30px; display: grid; place-items: center; background: none; border: none; border-radius: 8px; color: var(--ink-soft); font-size: 15px; transition: background .14s ease, color .14s ease; }
  header .x:hover { background: var(--ink-bg-hover); color: var(--ink); }
  header .x:focus-visible { outline: none; box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 30%, transparent); }

  .body { flex: 1; display: grid; grid-template-columns: 230px 1fr; overflow: hidden; }

  .rail { border-right: 1px solid var(--border-soft, var(--hairline)); padding: 10px; display: flex; flex-direction: column; gap: 3px; overflow-y: auto; background: color-mix(in srgb, var(--ink) 3%, transparent); }
  .botrow { display: flex; align-items: center; gap: 9px; text-align: left; padding: 7px 9px; border: none; background: transparent; border-radius: 9px; color: var(--ink-soft); }
  .botrow .bn { font-size: 13px; font-weight: 550; flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .bdot { width: 8px; height: 8px; flex: 0 0 auto; border-radius: 50%; background: var(--muted); }
  .bdot.on { background: var(--online, var(--success)); }
  .botrow:hover { background: color-mix(in srgb, var(--ink) 5%, transparent); }
  .botrow.sel { background: color-mix(in srgb, var(--accent) 14%, transparent); color: var(--ink); }
  .add { text-align: center; padding: 8px 10px; border: 1px dashed var(--hairline); background: transparent; border-radius: 9px; color: var(--ink-soft); margin-top: 4px; }
  .add:hover { border-color: color-mix(in srgb, var(--accent) 45%, var(--hairline)); color: var(--accent-strong, var(--accent)); }
  .botrow:focus-visible, .add:focus-visible { outline: none; box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 30%, transparent); }

  .pane { display: flex; flex-direction: column; min-width: 0; overflow: hidden; }
  .empty { display: grid; place-items: center; height: 100%; color: var(--ink-faint); font-size: 13px; line-height: 1.6; padding: 32px; text-align: center; }
  .empty strong { color: var(--ink); font-weight: 600; }

  .seg { display: inline-flex; align-self: flex-start; margin: 16px 18px 0; background: color-mix(in srgb, var(--ink) 5%, transparent); border-radius: 10px; padding: 3px; }
  .seg button { padding: 6px 14px; border: none; background: transparent; border-radius: 8px; font-size: 13px; color: var(--ink-soft); transition: color .14s ease, background .14s ease; }
  .seg button.on { background: var(--surface); color: var(--ink); box-shadow: var(--elev-1, 0 1px 2px rgba(0,0,0,0.08)); }
  .seg button:not(.on):hover { color: var(--ink); }
  .seg button:focus-visible { outline: none; box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 30%, transparent); }

  .content { flex: 1; overflow-y: auto; padding: 18px 20px; }

  footer { display: flex; align-items: center; gap: 10px; padding: 12px 18px; border-top: 1px solid var(--hairline); }
  footer .spacer { flex: 1; }
  footer button { padding: 8px 18px; border-radius: 10px; border: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 4%, var(--surface)); color: var(--ink); font-weight: 550; transition: background .14s ease, transform .12s ease, box-shadow .14s ease; }
  footer button:hover:not(:disabled) { background: color-mix(in srgb, var(--ink) 8%, var(--surface)); }
  footer button:active:not(:disabled) { transform: translateY(1px); }
  footer button:disabled { opacity: .5; cursor: default; transform: none; box-shadow: none; }
  footer .primary { background: linear-gradient(135deg, var(--grad-a), var(--grad-b)); color: #fff; border: none; box-shadow: 0 4px 14px color-mix(in srgb, var(--grad-a) 45%, transparent); }
  footer .primary:hover:not(:disabled) { transform: translateY(-1px); box-shadow: 0 8px 22px color-mix(in srgb, var(--grad-a) 52%, transparent); }
  footer .err { color: var(--danger); font-size: 12px; }
  footer .ok { color: var(--online); font-size: 12px; }
  footer button:focus-visible { outline: none; box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 30%, transparent); }

  .wizscrim { position: absolute; inset: 0; z-index: 60; background: color-mix(in srgb, #000 38%, transparent); display: flex; align-items: center; justify-content: center; }
  .wizcard { width: min(440px, 92%); background: var(--surface); border: 1px solid var(--hairline); border-radius: 14px; box-shadow: 0 18px 50px color-mix(in srgb, #000 38%, transparent); padding: 18px 20px; display: flex; flex-direction: column; gap: 14px; }
  .wizhead { display: flex; align-items: center; }
  .wizhead h3 { margin: 0; font-size: 15px; font-weight: 650; color: var(--ink); flex: 1; }
  .wizhead .x { width: 26px; height: 26px; border-radius: 8px; border: none; background: transparent; color: var(--ink-soft); font-size: 14px; }
  .wizhead .x:hover { background: color-mix(in srgb, var(--ink) 8%, transparent); }
  .wizsub { margin: 0; font-size: 12px; color: var(--ink-faint); line-height: 1.5; }
  .wizform { display: flex; flex-direction: column; gap: 12px; }
  .wizform label { display: flex; flex-direction: column; gap: 5px; font-size: 12px; font-weight: 550; color: var(--ink-soft); }
  .wizform input { background: color-mix(in srgb, var(--ink) 4%, var(--surface)); border: 1px solid var(--hairline); border-radius: 10px; padding: 8px 11px; color: var(--ink); font-size: 13px; outline: none; }
  .wizform input:focus { border-color: color-mix(in srgb, var(--accent) 55%, var(--hairline)); box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 16%, transparent); }
  .wizform small { color: var(--ink-faint); font-size: 11px; }
  .wizerr { color: var(--danger); font-size: 12px; }
  .wizbtns { display: flex; align-items: center; gap: 10px; }
  .wizbtns .spacer { flex: 1; }
  .wizbtns button { padding: 8px 16px; border-radius: 10px; border: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 4%, var(--surface)); color: var(--ink); font-weight: 550; }
  .wizbtns button:hover { background: color-mix(in srgb, var(--ink) 8%, var(--surface)); }
  .wizbtns button:disabled { opacity: .5; cursor: default; }
  .wizbtns .link { border: none; background: transparent; color: var(--ink-soft); font-weight: 500; font-size: 12px; padding: 8px 4px; }
  .wizbtns .link:hover { background: transparent; color: var(--accent-strong, var(--accent)); text-decoration: underline; }
  .wizbtns .primary { background: linear-gradient(135deg, var(--grad-a), var(--grad-b)); color: #fff; border: none; box-shadow: 0 4px 14px color-mix(in srgb, var(--grad-a) 45%, transparent); }
  .wizbtns button:focus-visible, .wizhead .x:focus-visible { outline: none; box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 30%, transparent); }
</style>
