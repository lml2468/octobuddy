<script lang="ts">
  import { store } from "../store.svelte";
</script>

<div class="conversations">
  <div class="bar" style="--wails-draggable: drag;">
    <span class="title">Conversations</span>
  </div>
  {#if !store.currentBot}
    <div class="empty">Pick a bot on the left.</div>
  {:else if store.botSessions.length === 0}
    <div class="empty">No conversations yet — say hello below.</div>
  {:else}
    <div class="list">
      {#each store.botSessions as s (s.key)}
        <button class="conv" class:sel={s.key === store.selectedKey} onclick={() => store.selectSession(s.key)}>
          <span class="ic">{s.title.startsWith("Console") ? "▣" : "❝"}</span>
          <span class="meta">
            <span class="ct">{s.title}</span>
            <span class="cs">{s.awaiting ? "replying…" : (s.messages.at(-1)?.text.slice(0, 48) || "No messages yet")}</span>
          </span>
        </button>
      {/each}
    </div>
  {/if}
</div>

<style>
  .conversations { display: flex; flex-direction: column; height: 100%; }
  .bar { padding: 30px 16px 10px; border-bottom: 1px solid var(--hairline); }
  .title { font-family: var(--serif); font-weight: 600; font-size: 14px; }
  .empty { color: var(--ink-faint); font-size: 12px; padding: 14px 16px; }
  .list { display: flex; flex-direction: column; gap: 2px; padding: 8px; overflow-y: auto; }

  .conv { display: flex; gap: 9px; align-items: center; width: 100%; text-align: left; background: transparent; border: none; padding: 8px; border-radius: var(--radius-sm); color: var(--ink); }
  .conv:hover { background: color-mix(in srgb, var(--ink) 5%, transparent); }
  .conv.sel { background: color-mix(in srgb, var(--brand) 16%, transparent); }
  .ic { color: var(--brand); width: 16px; text-align: center; flex: 0 0 16px; }
  .meta { display: flex; flex-direction: column; min-width: 0; }
  .ct { font-weight: 600; font-size: 13px; }
  .cs { font-size: 11px; color: var(--ink-soft); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
