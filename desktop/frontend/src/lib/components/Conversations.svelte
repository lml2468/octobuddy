<script lang="ts">
  import { store } from "../store.svelte";
  import Avatar from "./Avatar.svelte";

  let q = $state("");

  const sessions = $derived(
    store.botSessions.filter(
      (s) =>
        !q.trim() ||
        s.title.toLowerCase().includes(q.toLowerCase()) ||
        (s.messages.at(-1)?.text ?? "").toLowerCase().includes(q.toLowerCase()),
    ),
  );

  function relTime(ms: number): string {
    if (!ms) return "";
    const d = (Date.now() - ms) / 1000;
    if (d < 60) return "now";
    if (d < 3600) return `${Math.floor(d / 60)}m`;
    if (d < 86400) return `${Math.floor(d / 3600)}h`;
    return `${Math.floor(d / 86400)}d`;
  }
  const preview = (s: any) => (s.awaiting ? "replying…" : (s.messages.at(-1)?.text ?? "No messages yet"));
</script>

<div class="list-col">
  <div class="top" style="--wails-draggable: drag;">
    <div class="search" style="--wails-draggable: no-drag;">
      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><circle cx="11" cy="11" r="7"/><path d="m21 21-4.3-4.3"/></svg>
      <input placeholder="Search" bind:value={q} />
    </div>
  </div>

  {#if !store.currentBot}
    <div class="empty">Pick a bot.</div>
  {:else if sessions.length === 0}
    <div class="empty">{q ? "No matches." : "No conversations yet — say hello."}</div>
  {:else}
    <div class="rows">
      {#each sessions as s (s.key)}
        <button class="row" class:sel={s.key === store.selectedKey} onclick={() => store.selectSession(s.key)}>
          <Avatar name={s.title} size={48} />
          <div class="mid">
            <div class="r1">
              <span class="name">{s.title}</span>
              <span class="time">{relTime(s.lastActivity)}</span>
            </div>
            <div class="r2">
              <span class="preview" class:replying={s.awaiting}>{preview(s)}</span>
            </div>
          </div>
        </button>
      {/each}
    </div>
  {/if}
</div>

<style>
  .list-col { display: flex; flex-direction: column; height: 100%; background: var(--list); }

  .top {
    height: var(--header-h); flex: 0 0 var(--header-h);
    display: flex; align-items: center; padding: 0 12px;
  }
  .search {
    flex: 1; display: flex; align-items: center; gap: 7px;
    height: 30px; padding: 0 10px; border-radius: 8px;
    background: color-mix(in srgb, var(--ink) 7%, transparent);
    border: 1px solid transparent;
    color: var(--ink-faint);
    transition: border-color 0.15s ease, background 0.15s ease;
  }
  .search:focus-within { border-color: color-mix(in srgb, var(--accent) 55%, transparent); background: color-mix(in srgb, var(--ink) 4%, transparent); }
  .search input { flex: 1; border: none; background: transparent; outline: none; color: var(--ink); font-size: 13px; }
  .search input::placeholder { color: var(--ink-faint); }

  .empty { color: var(--ink-faint); font-size: 12px; padding: 16px; }
  .rows { flex: 1; overflow-y: auto; padding: 2px 8px 8px; }

  .row {
    display: flex; align-items: center; gap: 12px; width: 100%;
    height: var(--row-h); padding: 0 12px; border: none; background: transparent;
    border-radius: 10px; text-align: left; color: var(--ink);
  }
  .row:hover { background: color-mix(in srgb, var(--ink) 4%, transparent); }
  .row.sel, .row.sel:hover { background: var(--list-sel); }

  .mid { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 4px; }
  .r1 { display: flex; align-items: baseline; gap: 8px; }
  .name { font-size: 15px; font-weight: 600; flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .time { font-size: 11px; color: var(--ink-faint); flex: 0 0 auto; font-variant-numeric: tabular-nums; font-family: var(--mono); }
  .r2 { display: flex; }
  .preview { font-size: 13px; color: var(--ink-soft); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 100%; }
  .preview.replying { color: var(--accent); }

  /* Green selected row → dark text (WeChat). */
  .row.sel .name { color: var(--sel-ink); }
  .row.sel .time { color: color-mix(in srgb, var(--sel-ink) 70%, transparent); }
  .row.sel .preview { color: color-mix(in srgb, var(--sel-ink) 80%, transparent); }
  .row.sel .preview.replying { color: var(--sel-ink); }
</style>
