<script lang="ts">
  import { store } from "../store.svelte";
  import Bubble from "./Bubble.svelte";
  import EmptyState from "./EmptyState.svelte";

  let { onpick }: { onpick: (prompt: string) => void } = $props();

  let scroller: HTMLDivElement;
  let atBottom = $state(true);

  const session = $derived(store.currentSession);
  const messages = $derived(session?.messages ?? []);
  // A cheap signal that changes as content streams in (last message length + count).
  const tick = $derived(messages.length + (messages.at(-1)?.text.length ?? 0));

  function onScroll() {
    if (!scroller) return;
    atBottom = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 120;
  }

  // Follow streamed/added content only while parked near the bottom.
  $effect(() => {
    tick;
    if (atBottom && scroller) {
      requestAnimationFrame(() => { scroller.scrollTop = scroller.scrollHeight; });
    }
  });

  // Jump to bottom when switching sessions.
  $effect(() => {
    store.selectedKey;
    atBottom = true;
    requestAnimationFrame(() => { if (scroller) scroller.scrollTop = scroller.scrollHeight; });
  });
</script>

<div class="scroller" bind:this={scroller} onscroll={onScroll}>
  <div class="stack">
    {#if store.lastError}
      <div class="err">⚠️ {store.lastError}</div>
    {/if}

    {#if messages.length === 0}
      <EmptyState {onpick} />
    {:else}
      {#each messages as m (m.id)}
        <Bubble message={m} />
      {/each}
      {#if session?.awaiting}
        <div class="typing">
          <div class="avatar bot" aria-hidden="true"></div>
          <div class="dots"><span></span><span></span><span></span></div>
        </div>
      {/if}
      {#if session && session.outputTokens > 0}
        <div class="tokens">{session.inputTokens} in · {session.outputTokens} out</div>
      {/if}
    {/if}
  </div>
</div>

<style>
  .scroller { flex: 1; overflow-y: auto; }
  .stack { display: flex; flex-direction: column; gap: 12px; padding: 22px 24px 8px; max-width: 900px; margin: 0 auto; }

  .err {
    color: var(--danger); font-size: 12px;
    background: color-mix(in srgb, var(--danger) 10%, transparent);
    border-radius: var(--radius-sm); padding: 8px 12px;
  }

  .typing { display: flex; gap: 9px; align-items: center; }
  .avatar.bot { width: 28px; height: 28px; border-radius: 50%; background: radial-gradient(circle at 38% 32%, color-mix(in srgb, var(--brand) 75%, var(--paper-raised)), var(--brand)); box-shadow: var(--shadow-feather); }
  .dots { display: inline-flex; gap: 5px; padding: 12px 16px; background: var(--paper-raised); border: 1px solid var(--hairline); border-radius: var(--radius); box-shadow: var(--shadow-feather); }
  .dots span { width: 6px; height: 6px; border-radius: 50%; background: var(--ink-faint); animation: bounce 1.2s infinite ease-in-out; }
  .dots span:nth-child(2) { animation-delay: 0.15s; }
  .dots span:nth-child(3) { animation-delay: 0.3s; }
  @keyframes bounce { 0%, 60%, 100% { transform: translateY(0); opacity: 0.4; } 30% { transform: translateY(-4px); opacity: 1; } }

  .tokens { text-align: center; font-size: 11px; color: var(--ink-faint); font-variant-numeric: tabular-nums; padding-top: 2px; }
</style>
