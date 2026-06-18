<script lang="ts">
  import "./lib/styles/theme.css";
  import "./lib/styles/markdown.css";
  import { Events } from "@wailsio/runtime";
  import { store } from "./lib/store.svelte";
  import Sidebar from "./lib/components/Sidebar.svelte";
  import CommandPalette from "./lib/components/CommandPalette.svelte";
  import Transcript from "./lib/components/Transcript.svelte";
  import StatusBar from "./lib/components/StatusBar.svelte";
  import Composer from "./lib/components/Composer.svelte";
  import ConfigEditor from "./lib/components/ConfigEditor.svelte";
  import TrafficLights from "./lib/components/TrafficLights.svelte";
  import SkillsPanel from "./lib/components/SkillsPanel.svelte";
  import WorkflowsPanel from "./lib/components/WorkflowsPanel.svelte";
  import WorkspacePanel from "./lib/components/WorkspacePanel.svelte";
  import FilePreview from "./lib/components/FilePreview.svelte";
  import TokenUsage from "./lib/components/TokenUsage.svelte";

  let composer: Composer;
  let showEditor = $state(new URLSearchParams(location.search).has("editor"));
  let showSkills = $state(new URLSearchParams(location.search).has("skills"));
  let showWorkflows = $state(new URLSearchParams(location.search).has("workflows"));
  let showUsage = $state(new URLSearchParams(location.search).has("usage"));
  let showFiles = $state(new URLSearchParams(location.search).has("files"));
  let showPalette = $state(false);
  let collapsed = $state(false);
  // The file open in the wide preview pane (which overlays the chat). Null = chat.
  let previewPath = $state<string | null>(null);

  // The preview path belongs to one session's workspace; clear it when the
  // selected bot/session changes, or it would render the old file against the
  // new session (a not-found error, or the wrong same-named file).
  $effect(() => {
    store.selectedBotId; store.selectedKey;
    previewPath = null;
  });

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

  // ⌘K / Ctrl-K toggles the command palette (capture phase + both targets so
  // iframe focus quirks don't swallow it).
  function onKey(e: KeyboardEvent) {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
      e.preventDefault();
      showPalette = !showPalette;
    } else if (e.key === "Escape" && showPalette) {
      showPalette = false;
    }
  }
  try { window.addEventListener("keydown", onKey, true); document.addEventListener("keydown", onKey, true); } catch {}

  // Tray / gear menu open these as modals over the console (guarded: the Wails
  // runtime is absent in a plain browser, e.g. the headless UI-audit harness).
  try { Events.On("xclaw:open-editor", () => (showEditor = true)); } catch {}
  try { Events.On("xclaw:open-skills", () => (showSkills = true)); } catch {}
  try { Events.On("xclaw:open-workflows", () => (showWorkflows = true)); } catch {}
  try { Events.On("xclaw:open-usage", () => (showUsage = true)); } catch {}

  function pick(prompt: string) { composer?.setDraft(prompt); }
</script>

<TrafficLights />
<div class="shell">
  <Sidebar
    onedit={() => (showEditor = true)}
    onskills={() => (showSkills = true)}
    onworkflows={() => (showWorkflows = true)}
    onusage={() => (showUsage = true)}
    onpalette={() => (showPalette = true)}
    {collapsed}
    ontoggle={() => (collapsed = !collapsed)}
  />
  <div class="content">
    {#if previewPath && showFiles && store.currentSession}
      <FilePreview botId={store.selectedBotId} sessionKey={store.selectedKey} path={previewPath} onclose={() => (previewPath = null)} />
    {:else}
      <main class="chat">
        <header class="chat-bar" style="--wails-draggable: drag;">
          <span class="title">{store.currentSession?.title ?? "XClaw"}</span>
          <span class="spacer"></span>
          {#if store.currentBot}
            <button class="icon" class:on={showFiles} style="--wails-draggable: no-drag;" title="Workspace files" onclick={() => (showFiles = !showFiles)} aria-label="Toggle workspace" aria-pressed={showFiles}>
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M15 4v16"/></svg>
            </button>
          {/if}
        </header>
        <Transcript onpick={pick} />
        <StatusBar />
        <Composer bind:this={composer} />
      </main>
    {/if}
    {#if showFiles && store.currentSession}
      <aside class="files">
        <WorkspacePanel
          botId={store.selectedBotId}
          sessionKey={store.selectedKey}
          activePath={previewPath}
          onopen={(p) => (previewPath = p)}
          onclose={() => { showFiles = false; previewPath = null; }}
        />
      </aside>
    {/if}
  </div>
</div>

{#if showPalette}
  <CommandPalette
    onclose={() => (showPalette = false)}
    onedit={() => (showEditor = true)}
    onskills={() => (showSkills = true)}
    onworkflows={() => (showWorkflows = true)}
    onusage={() => (showUsage = true)}
  />
{/if}
{#if showEditor}
  <ConfigEditor onclose={() => (showEditor = false)} onskills={() => (showSkills = true)} onusage={() => (showUsage = true)} />
{/if}
{#if showSkills}
  <SkillsPanel onclose={() => (showSkills = false)} />
{/if}
{#if showWorkflows}
  <WorkflowsPanel onclose={() => (showWorkflows = false)} />
{/if}
{#if showUsage}
  <TokenUsage onclose={() => (showUsage = false)} />
{/if}

<style>
  .shell { display: flex; height: 100vh; background: var(--window-grad); }
  /* Custom window controls for the frameless window — top-left over the sidebar. */
  :global(.lights) { position: fixed; top: 14px; left: 15px; z-index: 1000; }

  /* The chat lives in a bright frosted card; the translucent sidebar (and the
     window gradient behind it) read as the scenery around it. */
  .content {
    flex: 1; min-width: 0; display: flex;
    margin: 4px 4px 4px 0;
    border: 1px solid var(--glass-border);
    border-radius: 16px;
    overflow: hidden;
    box-shadow: var(--elev-2);
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
  .spacer { flex: 1; }
  .icon {
    display: inline-flex; align-items: center; justify-content: center;
    width: 32px; height: 32px; border-radius: 8px; border: none;
    background: transparent; color: var(--ink-soft);
    transition: background 0.14s ease, color 0.14s ease;
  }
  .icon:hover { background: color-mix(in srgb, var(--ink) 7%, transparent); color: var(--accent); }
  .icon.on { background: color-mix(in srgb, var(--accent) 12%, transparent); color: var(--accent); }
</style>
