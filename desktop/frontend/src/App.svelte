<script lang="ts">
  import "./lib/styles/theme.css";
  import { Events } from "@wailsio/runtime";
  import { store } from "./lib/store.svelte";
  import Rail from "./lib/components/Rail.svelte";
  import Conversations from "./lib/components/Conversations.svelte";
  import Transcript from "./lib/components/Transcript.svelte";
  import Composer from "./lib/components/Composer.svelte";
  import ConfigEditor from "./lib/components/ConfigEditor.svelte";
  import TrafficLights from "./lib/components/TrafficLights.svelte";
  import SkillsPanel from "./lib/components/SkillsPanel.svelte";
  import WorkflowsPanel from "./lib/components/WorkflowsPanel.svelte";
  import WorkspacePanel from "./lib/components/WorkspacePanel.svelte";

  let composer: Composer;
  let showEditor = $state(new URLSearchParams(location.search).has("editor"));
  let showSkills = $state(new URLSearchParams(location.search).has("skills"));
  let showWorkflows = $state(new URLSearchParams(location.search).has("workflows"));
  let showFiles = $state(new URLSearchParams(location.search).has("files"));

  // Tray / gear menu open these as modals over the console (guarded: the Wails
  // runtime is absent in a plain browser, e.g. the headless UI-audit harness).
  try { Events.On("xclaw:open-editor", () => (showEditor = true)); } catch {}
  try { Events.On("xclaw:open-skills", () => (showSkills = true)); } catch {}
  try { Events.On("xclaw:open-workflows", () => (showWorkflows = true)); } catch {}

  function pick(prompt: string) { composer?.setDraft(prompt); }
</script>

<TrafficLights />
<div class="shell">
  <Rail onedit={() => (showEditor = true)} onskills={() => (showSkills = true)} onworkflows={() => (showWorkflows = true)} />
  <div class="content">
    <section class="list"><Conversations /></section>
    <main class="chat">
      <header class="chat-bar" style="--wails-draggable: drag;">
        <span class="title">{store.currentSession?.title ?? "XClaw"}</span>
        <span class="spacer"></span>
        {#if store.currentBot}
          <button class="icon" class:on={showFiles} style="--wails-draggable: no-drag;" title="Workspace files" onclick={() => (showFiles = !showFiles)} aria-label="Toggle workspace" aria-pressed={showFiles}>
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M15 4v16"/></svg>
          </button>
          <button class="icon" style="--wails-draggable: no-drag;" title="Clear conversation memory" onclick={() => store.reset()} aria-label="Clear memory">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2m-9 0v14a2 2 0 0 0 2 2h6a2 2 0 0 0 2-2V6"/></svg>
          </button>
          <button class="icon" style="--wails-draggable: no-drag;" title="Restart core" onclick={() => store.restartCore()} aria-label="Restart core">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12a9 9 0 1 0 3-6.7L3 8"/><path d="M3 3v5h5"/></svg>
          </button>
        {/if}
      </header>
      <Transcript onpick={pick} />
      <Composer bind:this={composer} />
    </main>
    {#if showFiles && store.currentSession}
      <aside class="files">
        <WorkspacePanel botId={store.selectedBotId} sessionKey={store.selectedKey} onclose={() => (showFiles = false)} />
      </aside>
    {/if}
  </div>
</div>

{#if showEditor}
  <ConfigEditor onclose={() => (showEditor = false)} />
{/if}
{#if showSkills}
  <SkillsPanel onclose={() => (showSkills = false)} />
{/if}
{#if showWorkflows}
  <WorkflowsPanel onclose={() => (showWorkflows = false)} />
{/if}

<style>
  .shell { display: flex; height: 100vh; background: var(--rail); }
  /* Custom window controls for the frameless window — top-left over the rail. */
  :global(.lights) { position: fixed; top: 14px; left: 15px; z-index: 1000; }

  /* Content panel: list + chat float in a rounded, bordered card inset from the
     window edges, so the dark rail reads as a thin frame around it. */
  .content {
    flex: 1; min-width: 0; display: flex;
    margin: 4px 4px 4px 3px;
    border: 1px solid var(--panel-border);
    border-radius: 9px;
    overflow: hidden;
    box-shadow: var(--elev-2);
  }
  .list { width: var(--list-w); flex: 0 0 var(--list-w); height: 100%; border-right: 1px solid var(--hairline); overflow: hidden; }
  .chat { flex: 1; min-width: 0; height: 100%; display: flex; flex-direction: column; background: radial-gradient(130% 90% at 50% 0%, color-mix(in srgb, var(--surface) 22%, var(--chat)) 0%, var(--chat) 58%); }
  /* Workspace sidebar: third column inside the rounded content card (no own
     radius/overflow — it lives inside .content's clip). Left hairline mirrors
     the list's right hairline; .chat is flex:1 so it shrinks (push/split). */
  .files { width: 320px; flex: 0 0 320px; height: 100%; border-left: 1px solid var(--hairline); background: var(--surface); overflow: hidden; display: flex; flex-direction: column; }

  .chat-bar {
    height: var(--header-h); flex: 0 0 var(--header-h);
    display: flex; align-items: center; gap: 6px;
    padding: 0 var(--gutter);
    background: var(--surface); border-bottom: 1px solid var(--hairline);
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
