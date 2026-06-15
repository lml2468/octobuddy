<script lang="ts">
  import { store } from "../store.svelte";
  import Octopus from "./Octopus.svelte";

  let { onedit }: { onedit: () => void } = $props();
</script>

<div class="sidebar">
  <div class="identity" style="--wails-draggable: drag;">
    <span class="mark"><Octopus size={20} /></span>
    <span class="word">XClaw</span>
    <button class="gear" style="--wails-draggable: no-drag;" onclick={onedit} title="Edit bots" aria-label="Edit bots">⚙</button>
  </div>

  <div class="section">Bots</div>
  <div class="list">
    {#if store.bots.length === 0}
      <div class="empty">No bots configured</div>
    {/if}
    {#each store.bots as b (b.id)}
      <button class="bot" class:sel={b.id === store.selectedBotId} onclick={() => store.selectBot(b.id)}>
        <span class="dot" class:on={b.connected}></span>
        <span class="meta">
          <span class="id">{b.id}</span>
          <span class="status">{b.connected ? "connected" : (b.lastError ?? "offline")}</span>
        </span>
      </button>
    {/each}
  </div>
</div>

<style>
  .sidebar { display: flex; flex-direction: column; height: 100%; }
  .identity {
    display: flex; align-items: center; gap: 9px;
    padding: 30px 16px 12px; color: var(--brand);
    border-bottom: 1px solid var(--hairline);
  }
  .word { font-family: var(--serif); font-size: 17px; font-weight: 600; color: var(--ink); flex: 1; }
  .gear { background: none; border: none; color: var(--ink-faint); font-size: 14px; padding: 2px 4px; border-radius: 6px; }
  .gear:hover { color: var(--brand); }
  .section { font-size: 10px; letter-spacing: 0.8px; text-transform: uppercase; color: var(--ink-faint); padding: 14px 16px 6px; }
  .list { display: flex; flex-direction: column; gap: 2px; padding: 0 8px; overflow-y: auto; }
  .empty { color: var(--ink-faint); font-size: 12px; padding: 4px 8px; }

  .bot {
    display: flex; align-items: center; gap: 9px;
    width: 100%; text-align: left; background: transparent; border: none;
    padding: 7px 8px; border-radius: var(--radius-sm); color: var(--ink);
  }
  .bot:hover { background: color-mix(in srgb, var(--ink) 5%, transparent); }
  .bot.sel { background: color-mix(in srgb, var(--brand) 16%, transparent); }
  .dot { width: 8px; height: 8px; border-radius: 50%; background: var(--ink-faint); flex: 0 0 8px; }
  .dot.on { background: #5aa873; box-shadow: 0 0 6px color-mix(in srgb, #5aa873 60%, transparent); }
  .meta { display: flex; flex-direction: column; min-width: 0; }
  .id { font-weight: 600; font-size: 13px; }
  .status { font-size: 11px; color: var(--ink-soft); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
