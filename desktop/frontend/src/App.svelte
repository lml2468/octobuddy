<script lang="ts">
  import "./lib/styles/theme.css";
  import "./lib/styles/markdown.css";
  import { Events } from "@wailsio/runtime";
  import { store } from "./lib/store.svelte";
  import Sidebar from "./lib/components/Sidebar.svelte";
  import Transcript from "./lib/components/Transcript.svelte";
  import StatusBar from "./lib/components/StatusBar.svelte";
  import Composer from "./lib/components/Composer.svelte";
  import TrafficLights from "./lib/components/TrafficLights.svelte";
  import WorkspacePanel from "./lib/components/WorkspacePanel.svelte";
  import FilePreview from "./lib/components/FilePreview.svelte";
  import { confirm } from "./lib/confirm.svelte";

  // Lazy-loaded chunks: these UIs are only reached via tray menu, ⌘K, or
  // explicit sidebar click — they don't belong in the first-paint critical
  // path. Vite splits each into its own chunk; the initial index.js drops
  // by ~the combined size of these components and their sub-trees
  // (SettingsModal alone owns 5 panes incl. SchedulesPane). The import
  // resolves before showSettings flips, so the modal never paints empty.
  let SettingsModalCmp = $state<any>(null);
  let TokenUsageCmp = $state<any>(null);
  let CommandPaletteCmp = $state<any>(null);

  type FileSource = "workspace" | "memory";

  let composer = $state<Composer>();

 // Initial-show + initial-tab parsing from ?settings[=basic|octo|skills|workflows].
  type SettingsTab = "basic" | "octo" | "skills" | "workflows";
  const TABS: SettingsTab[] = ["basic", "octo", "skills", "workflows"];
  function initialSettingsState(): { show: boolean; tab: SettingsTab } {
    const t = new URLSearchParams(location.search).get("settings");
    if (t === null) return { show: false, tab: "basic" };
    return { show: true, tab: TABS.includes(t as SettingsTab) ? (t as SettingsTab) : "basic" };
  }
  const initialSettings = initialSettingsState();
  let showSettings = $state(initialSettings.show);
  let settingsTab = $state<SettingsTab>(initialSettings.tab);
 // First-run path: Sidebar's "+ 新增 Bot" CTA flips this so SettingsModal
 // opens the wizard immediately. Reset after every modal close to avoid
 // re-triggering on subsequent gear-icon opens.
  let settingsOpenWizard = $state(false);
  let showUsage = $state(new URLSearchParams(location.search).has("usage"));
  let filePane = $state<FileSource | null>(
    new URLSearchParams(location.search).has("memory")
      ? "memory"
      : new URLSearchParams(location.search).has("files")
        ? "workspace"
        : null,
  );
  let showPalette = $state(false);
  let collapsed = $state(false);
 // The file open in the wide preview pane (which overlays the chat). Null = chat.
  let previewPath = $state<string | null>(null);

  // Preload the lazy chunks when the initial URL asks for them, so the
  // first paint isn't a blank screen waiting on import().
  $effect(() => {
    if (initialSettings.show && !SettingsModalCmp) {
      void loadSettingsModal();
    }
    if (showUsage && !TokenUsageCmp) {
      void loadTokenUsage();
    }
  });

  async function loadSettingsModal() {
    if (SettingsModalCmp) return;
    SettingsModalCmp = (await import("./lib/components/SettingsModal.svelte")).default;
  }
  async function loadTokenUsage() {
    if (TokenUsageCmp) return;
    TokenUsageCmp = (await import("./lib/components/TokenUsage.svelte")).default;
  }
  async function loadCommandPalette() {
    if (CommandPaletteCmp) return;
    CommandPaletteCmp = (await import("./lib/components/CommandPalette.svelte")).default;
  }

 // The preview path belongs to one session's workspace; clear it when the
 // selected bot/session changes, or it would render the old file against the
 // new session (a not-found error, or the wrong same-named file).
  $effect(() => {
    store.selectedBotId; store.selectedKey;
    previewPath = null;
  });

  function toggleFilePane(source: FileSource) {
    filePane = filePane === source ? null : source;
    previewPath = null;
  }

 // Per-Space theme color (Arc's signature): each bot carries an Arc theme
 // gradient, chosen deterministically from its id, and the whole window
 // backdrop blooms from it. Selecting a bot re-themes the window; light and
 // dark both recompute since --window-grad references --grad-a/--grad-b.
  const SPACE_THEMES: [string, string][] = [
    ["#ff7e5f", "#feb47b"], // Sunset — peach → coral
    ["#7f5af0", "#e84393"], // Twilight — violet → fuchsia
    ["#16f2b3", "#0db4f7"], // Aurora — mint → cyan
    ["#ff5f5f", "#ffb07c"], // Coral — brand → amber
    ["#5f8bff", "#7f5af0"], // Indigo → violet
  ];
  function spaceTheme(id: string): [string, string] {
    let h = 0;
    for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) | 0;
    return SPACE_THEMES[Math.abs(h) % SPACE_THEMES.length];
  }
  $effect(() => {
    const id = store.selectedBotId;
    if (!id) return;
    const [a, b] = spaceTheme(id);
    const s = document.documentElement.style;
    s.setProperty("--grad-a", a);
    s.setProperty("--grad-b", b);
  });

 // ⌘K / Ctrl-K toggles the command palette. Listen on document (NOT both
 // window AND document — registering on both fired onKey twice per
 // keydown, so the toggle was cancelling itself and the palette never
 // appeared) in the capture phase so iframe focus quirks don't swallow it.
 // Bound inside $effect with cleanup so dev-mode HMR doesn't stack a fresh
 // handler on every save.
  function onKey(e: KeyboardEvent) {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
      e.preventDefault();
      if (!showPalette) {
        void loadCommandPalette().then(() => { showPalette = true; });
      } else {
        showPalette = false;
      }
    } else if (e.key === "Escape" && showPalette) {
      showPalette = false;
    }
  }
  $effect(() => {
    document.addEventListener("keydown", onKey, true);
    return () => document.removeEventListener("keydown", onKey, true);
  });

 // Tray opens the unified Settings modal at a specific tab, or the standalone
 // Token Usage modal. Mutual exclusion: only one top-level modal at a time.
 // Both await the lazy chunk before flipping the show flag so the modal
 // never paints empty during the initial chunk fetch (~tens of ms one-time).
  async function openSettings(tab: SettingsTab = "basic", openWizard: boolean = false) {
    settingsTab = tab;
    settingsOpenWizard = openWizard;
    await loadSettingsModal();
    showSettings = true;
    showUsage = false;
  }
  async function openUsage() {
    await loadTokenUsage();
    showUsage = true;
    showSettings = false;
  }
 // Wails Events.On returns an unsubscribe — capture inside $effect so HMR
 // doesn't keep stacking handlers each save. Both subscriptions
 // share the same cleanup boundary.
  $effect(() => {
    const offSettings = Events.On("octobuddy:open-settings", (e: any) => {
      const tab = e?.data?.tab as SettingsTab | undefined;
      void openSettings(tab && TABS.includes(tab) ? tab : "basic");
    });
    const offUsage = Events.On("octobuddy:open-usage", () => void openUsage());
    return () => { try { offSettings?.(); offUsage?.(); } catch {} };
  });

  function pick(prompt: string) { composer?.setDraft(prompt); }

  async function resetSession() {
    if (!store.currentSession) return;
    const msg = store.isConsole
      ? "重置 Console 会话？将清空 resume id 与本地记录，下次发送从全新会话开始。"
      : "重置此 IM 会话？将清空 resume id 与本地记录，对端下条消息将开启全新会话。";
    if (await confirm({ message: msg, confirmLabel: "重置", danger: true })) store.reset();
  }
</script>

{#if !collapsed}
  <TrafficLights />
{/if}
<div class="shell">
  <Sidebar
    onedit={() => void openSettings("basic")}
    onaddbot={() => void openSettings("basic", true)}
    onusage={() => void openUsage()}
    onpalette={() => void loadCommandPalette().then(() => { showPalette = true; })}
    {collapsed}
  />
  <div class="content">
    {#if previewPath && filePane && store.currentSession}
      <FilePreview
        botId={store.selectedBotId}
        channelType={store.currentSession.channelType}
        sessionKey={store.selectedKey}
        source={filePane}
        path={previewPath}
        onclose={() => (previewPath = null)}
      />
    {:else}
      <main class="chat">
        <header class="chat-bar" style="--wails-draggable: drag;">
          <button class="icon sb-toggle" class:collapsed style="--wails-draggable: no-drag;" title={collapsed ? "展开侧栏" : "收起侧栏"} aria-label={collapsed ? "展开侧栏" : "收起侧栏"} aria-expanded={!collapsed} onclick={() => (collapsed = !collapsed)}>
            <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m14 6-6 6 6 6"/></svg>
          </button>
          <span class="title">{(() => {
            const s = store.currentSession;
            const own = s?.channelName || s?.title;
            if (!own) return "OctoBuddy";
            return s?.parentChannelName ? `${s.parentChannelName} > ${own}` : own;
          })()}</span>
          {#if store.currentSession && !store.isConsole}
            <span class="ro-badge" title="此会话来自 Octo IM，桌面仅供查看；用户消息从 IM 客户端发送">来自 IM · 只读</span>
          {/if}
          <span class="spacer"></span>
          {#if store.currentSession}
            <button class="icon" style="--wails-draggable: no-drag;" title="重置会话（清空 resume id 与本地记录）" aria-label="重置会话" onclick={resetSession}>
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12a9 9 0 1 0 3-6.7"/><path d="M3 4v5h5"/></svg>
            </button>
          {/if}
          {#if store.currentBot}
            <button class="icon" class:on={filePane === "memory"} style="--wails-draggable: no-drag;" title="Session memory" onclick={() => toggleFilePane("memory")} aria-label="Toggle memory" aria-pressed={filePane === "memory"}>
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M15 14c.2-1 .7-1.7 1.5-2.5 1-.9 1.5-2.2 1.5-3.5a6 6 0 0 0-12 0c0 1.3.5 2.6 1.5 3.5.7.8 1.3 1.5 1.5 2.5"/><path d="M9 18h6"/><path d="M10 22h4"/></svg>
            </button>
            <button class="icon" class:on={filePane === "workspace"} style="--wails-draggable: no-drag;" title="Workspace files" onclick={() => toggleFilePane("workspace")} aria-label="Toggle workspace" aria-pressed={filePane === "workspace"}>
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M15 4v16"/></svg>
            </button>
          {/if}
        </header>
        <Transcript onpick={pick} />
        <StatusBar />
        {#if store.isConsole}
          <Composer bind:this={composer} />
        {/if}
      </main>
    {/if}
    {#if filePane && store.currentSession}
      <aside class="files">
        <WorkspacePanel
          botId={store.selectedBotId}
          channelType={store.currentSession.channelType}
          sessionKey={store.selectedKey}
          source={filePane}
          activePath={previewPath}
          onopen={(p) => (previewPath = p)}
          onclose={() => { filePane = null; previewPath = null; }}
        />
      </aside>
    {/if}
  </div>
</div>

{#if showPalette && CommandPaletteCmp}
  <CommandPaletteCmp
    onclose={() => (showPalette = false)}
    onedit={() => void openSettings("basic")}
    onskills={() => void openSettings("skills")}
    onworkflows={() => void openSettings("workflows")}
    onusage={() => void openUsage()}
  />
{/if}
{#if showSettings && SettingsModalCmp}
  <SettingsModalCmp onclose={() => { showSettings = false; settingsOpenWizard = false; }} initialTab={settingsTab} openWizardOnMount={settingsOpenWizard} />
{/if}
{#if showUsage && TokenUsageCmp}
  <TokenUsageCmp onclose={() => (showUsage = false)} />
{/if}

<style>
  .shell { display: flex; height: 100vh; background: var(--window-grad); }
 /* Custom window controls for the frameless window — vertically centered in the header band, top-left over the sidebar. */
  :global(.lights) { position: fixed; top: calc((var(--header-h) - 12px) / 2); left: 15px; z-index: 1000; }

 /* The chat fills the right area flush — no card framing. */
  .content {
    flex: 1; min-width: 0; display: flex;
    overflow: hidden;
  }
  .chat { flex: 1; min-width: 0; height: 100%; display: flex; flex-direction: column; background: var(--glass); backdrop-filter: blur(24px) saturate(180%); -webkit-backdrop-filter: blur(24px) saturate(180%); }
  .files { width: 320px; flex: 0 0 320px; height: 100%; border-left: 1px solid var(--hairline); background: var(--glass); backdrop-filter: blur(24px) saturate(180%); -webkit-backdrop-filter: blur(24px) saturate(180%); overflow: hidden; display: flex; flex-direction: column; }

  .chat-bar {
    height: var(--header-h); flex: 0 0 var(--header-h);
    display: flex; align-items: center; gap: 6px;
    padding: 0 var(--gutter);
    background: color-mix(in srgb, var(--surface) 68%, transparent);
    backdrop-filter: blur(20px) saturate(160%); -webkit-backdrop-filter: blur(20px) saturate(160%);
    border-bottom: 1px solid var(--hairline);
  }
  .title { font-size: 15px; font-weight: 600; color: var(--ink); }
  .ro-badge {
    margin-left: 8px;
    padding: 2px 8px;
    border-radius: 999px;
    font-size: 11px; line-height: 1.4;
    color: var(--ink-soft);
    background: color-mix(in srgb, var(--ink) 6%, transparent);
    border: 1px solid var(--hairline);
    white-space: nowrap;
  }
  .spacer { flex: 1; }
  .icon {
    display: inline-flex; align-items: center; justify-content: center;
    width: 32px; height: 32px; border-radius: 8px; border: none;
    background: transparent; color: var(--ink-soft);
    transition: background 0.14s ease, color 0.14s ease;
  }
  .icon:hover { background: color-mix(in srgb, var(--ink) 7%, transparent); color: var(--accent); }
  .icon.on { background: color-mix(in srgb, var(--accent) 12%, transparent); color: var(--accent); }
 /* Sidebar collapse/expand toggle, top-left of the chat header. Chevron points
     toward the rail (collapse); flips outward when collapsed (expand). */
  .sb-toggle { margin-left: -4px; }
  .sb-toggle svg { transition: transform .2s var(--ease-standard, ease); }
  .sb-toggle.collapsed svg { transform: rotate(180deg); }
  .sb-toggle:active { transform: scale(0.9); }
  @media (prefers-reduced-motion: reduce) { .sb-toggle svg { transition: none; } .sb-toggle:active { transform: none; } }
</style>
