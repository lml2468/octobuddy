<script lang="ts">
  import { store } from "../store.svelte";
  import Bubble from "./Bubble.svelte";
  import Avatar from "./Avatar.svelte";
  import EmptyState from "./EmptyState.svelte";

  let { onpick }: { onpick: (prompt: string) => void } = $props();

  let scroller: HTMLDivElement;
  let atBottom = $state(true);

  const session = $derived(store.currentSession);
  const messages = $derived(session?.messages ?? []);
  // Bump on new messages, growing text, AND turn state so the view tracks the
  // working spinner appearing/disappearing too.
  const tick = $derived(messages.length + (messages.at(-1)?.text.length ?? 0) + (session?.awaiting ? 1 : 0));

  function onScroll() {
    if (!scroller) return;
    atBottom = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 120;
  }
  $effect(() => {
    tick;
    if (atBottom && scroller) requestAnimationFrame(() => { scroller.scrollTop = scroller.scrollHeight; });
  });
  $effect(() => {
    store.selectedKey;
    atBottom = true;
    requestAnimationFrame(() => { if (scroller) scroller.scrollTop = scroller.scrollHeight; });
  });
</script>

<div class="scroller" bind:this={scroller} onscroll={onScroll}>
  <div class="stack">
    {#if store.lastError}
      <div class="err">{store.lastError}</div>
    {/if}

    {#if messages.length === 0}
      <EmptyState {onpick} />
    {:else}
      {#each messages as m (m.id)}
        <Bubble message={m} />
      {/each}
      {#if session?.awaiting}
        <!-- The answer streams into the status box (process), not here. The chat
             shows a working indicator until the final answer lands at turn end. -->
        <div class="row">
          <Avatar octopus size={36} />
          <div class="typing"><span></span><span></span><span></span></div>
        </div>
      {/if}
      {#if session && session.outputTokens > 0}
        <div class="tokens">
          {session.inputTokens} in · {session.outputTokens} out{#if session.cachedInputTokens > 0} · {session.cachedInputTokens} cached{/if}{#if session.costUsd > 0} · ${session.costUsd.toFixed(4)}{/if}
        </div>
      {/if}
    {/if}
  </div>
</div>

<style>
  .scroller { flex: 1; overflow-y: auto; background: var(--chat); }
  .stack { display: flex; flex-direction: column; gap: 14px; padding: 22px var(--gutter, 28px) 12px; max-width: var(--content-max); width: 100%; margin: 0 auto; }

  .err { align-self: center; color: var(--danger); font-size: 12px; background: color-mix(in srgb, var(--danger) 12%, transparent); border-radius: 4px; padding: 7px 12px; }

  .row { display: flex; gap: 10px; align-items: flex-start; }
  .typing { display: inline-flex; gap: 5px; padding: 13px 14px; background: var(--in-bubble); border-radius: var(--bubble-radius); border-top-left-radius: 3px; box-shadow: 0 1px 1.5px rgba(20,22,28,0.08); }
  .typing span { width: 6px; height: 6px; border-radius: 50%; background: var(--ink-faint); animation: bounce 1.2s infinite ease-in-out; }
  .typing span:nth-child(2) { animation-delay: 0.15s; }
  .typing span:nth-child(3) { animation-delay: 0.3s; }
  @keyframes bounce { 0%, 60%, 100% { transform: translateY(0); opacity: 0.4; } 30% { transform: translateY(-4px); opacity: 1; } }

  .tokens { align-self: center; font-size: 11px; color: var(--ink-faint); font-variant-numeric: tabular-nums; padding-top: 2px; }
</style>
