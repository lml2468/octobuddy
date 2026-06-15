<script lang="ts">
  import "./lib/styles/theme.css";
  import { Events } from "@wailsio/runtime";
  import { store } from "./lib/store.svelte";
  import Canvas from "./lib/components/Canvas.svelte";
  import Sidebar from "./lib/components/Sidebar.svelte";
  import Conversations from "./lib/components/Conversations.svelte";
  import Transcript from "./lib/components/Transcript.svelte";
  import Composer from "./lib/components/Composer.svelte";
  import ConfigEditor from "./lib/components/ConfigEditor.svelte";

  let composer: Composer;
  let showEditor = $state(new URLSearchParams(location.search).has("editor"));

  // The tray "Edit Bots…" item opens the editor.
  Events.On("xclaw:open-editor", () => (showEditor = true));

  function pick(prompt: string) {
    composer?.setDraft(prompt);
  }
</script>

<Canvas />
<div class="shell">
  <aside class="col sidebar"><Sidebar onedit={() => (showEditor = true)} /></aside>
  <section class="col conversations"><Conversations /></section>
  <main class="col detail">
    <div class="detail-bar" style="--wails-draggable: drag;">
      <span class="dtitle">{store.currentSession?.title ?? ""}</span>
      <span class="spacer"></span>
      {#if store.currentBot}
        <button class="tool-btn" title="Clear conversation memory" onclick={() => store.reset()} aria-label="Clear memory">⌫</button>
        <button class="tool-btn" title="Restart core" onclick={() => store.restartCore()} aria-label="Restart core">⟳</button>
      {/if}
    </div>
    <Transcript onpick={pick} />
    <Composer bind:this={composer} />
  </main>
</div>

{#if showEditor}
  <ConfigEditor onclose={() => (showEditor = false)} />
{/if}

<style>
  .shell {
    position: relative;
    z-index: 1;
    display: grid;
    grid-template-columns: 200px 280px 1fr;
    height: 100vh;
  }
  .col { min-width: 0; height: 100vh; overflow: hidden; }
  /* Columns are translucent paper tints so the watercolor canvas breathes
     through them; hairlines give the contrast steps. */
  .sidebar { background: color-mix(in srgb, var(--paper) 72%, transparent); border-right: 1px solid var(--hairline); backdrop-filter: saturate(1.1); }
  .conversations { background: color-mix(in srgb, var(--paper) 58%, transparent); border-right: 1px solid var(--hairline); }
  .detail { display: flex; flex-direction: column; background: transparent; }

  .detail-bar { display: flex; align-items: center; gap: 8px; padding: 30px 16px 8px; }
  .dtitle { font-family: var(--serif); font-weight: 600; font-size: 14px; color: var(--ink-soft); }
  .spacer { flex: 1; }
  .tool-btn {
    width: 28px; height: 28px; border-radius: 8px; border: 1px solid var(--hairline);
    background: var(--paper-raised); color: var(--ink-soft); font-size: 14px; line-height: 1;
  }
  .tool-btn:hover { color: var(--ink); border-color: color-mix(in srgb, var(--brand) 40%, var(--hairline)); }
</style>
