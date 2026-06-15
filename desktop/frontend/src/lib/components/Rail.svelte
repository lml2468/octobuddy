<script lang="ts">
  import { store } from "../store.svelte";
  import Avatar from "./Avatar.svelte";
  import Octopus from "./Octopus.svelte";

  let { onedit }: { onedit: () => void } = $props();
</script>

<div class="rail">
  <div class="brand" style="--wails-draggable: drag;" title="XClaw">
    <Octopus size={30} />
  </div>

  <div class="bots">
    {#each store.bots as b (b.id)}
      <button
        class="slot"
        class:sel={b.id === store.selectedBotId}
        title={b.id + (b.connected ? " · connected" : " · offline")}
        onclick={() => store.selectBot(b.id)}
      >
        <span class="pill">
          <span class="tile">
            <Avatar name={b.id} size={40} />
            <span class="status" class:on={b.connected}></span>
          </span>
        </span>
      </button>
    {/each}
  </div>

  <div class="foot">
    <button class="icon" onclick={onedit} title="Edit bots" aria-label="Edit bots">
      <svg width="19" height="19" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 8 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H2a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 3.6 8a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H8a1.65 1.65 0 0 0 1-1.51V2a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V8a1.65 1.65 0 0 0 1.51 1H22a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>
    </button>
  </div>
</div>

<style>
  .rail {
    width: var(--rail-w); flex: 0 0 var(--rail-w); height: 100%;
    background: var(--rail); color: var(--rail-ink);
    display: flex; flex-direction: column; align-items: center;
    padding-bottom: 12px;
  }
  .brand {
    height: 72px; flex: 0 0 72px;
    display: flex; align-items: center; justify-content: center;
    padding-top: var(--titlebar);
    color: #fff;
  }
  .bots { width: var(--rail-w); flex: 1; overflow-y: auto; overflow-x: hidden; padding-top: 4px; scrollbar-width: none; }
  .bots::-webkit-scrollbar { width: 0; height: 0; }

  /* Block + margin:auto with an explicit width = bulletproof centering, no flex
     auto-size surprises. */
  .slot {
    display: block; width: 52px; margin: 4px auto; padding: 0;
    border: none; background: transparent; cursor: pointer;
  }
  .pill {
    width: 52px; height: 52px; display: grid; place-items: center;
    border-radius: 8px; transition: background 0.14s ease;
  }
  .slot:hover .pill { background: color-mix(in srgb, var(--rail-ink) 14%, transparent); }
  .slot.sel .pill { background: color-mix(in srgb, var(--accent) 24%, transparent); }

  .tile { position: relative; width: 40px; height: 40px; }
  .status {
    position: absolute; right: 0; bottom: 0; width: 12px; height: 12px;
    border-radius: 50%; background: var(--ink-faint);
    border: 2.5px solid var(--rail);
  }
  .slot.sel .status { border-color: var(--rail-active); }
  .status.on { background: var(--online); }

  .foot { padding-top: 8px; }
  .icon {
    width: 38px; height: 38px; border-radius: 8px; border: none; background: transparent;
    color: var(--rail-ink); display: flex; align-items: center; justify-content: center;
    transition: background 0.14s ease, color 0.14s ease;
  }
  .icon:hover { background: var(--rail-active); color: #fff; }
</style>
