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
  // Bump on new messages and on turn-state transitions (working spinner
  // appearing/disappearing). Replies arrive as whole-text pushes today, so
  // the array's length is the granularity that matters; if a streaming
  // text path is added later, mix in the last message's text length here.
  const tick = $derived(messages.length + (session?.awaiting ? 1 : 0));

  function onScroll() {
    if (!scroller) return;
    atBottom = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 120;
  }
  $effect(() => {
    tick;
    if (!atBottom || !scroller) return;
    // Cancel any prior queued frame so a burst of new messages collapses
    // to one scroll (round 12 F8); the cleanup also unpins the closure
    // when the component is destroyed mid-frame.
    const id = requestAnimationFrame(() => { scroller.scrollTop = scroller.scrollHeight; });
    return () => cancelAnimationFrame(id);
  });
  $effect(() => {
    store.selectedKey;
    atBottom = true;
    const id = requestAnimationFrame(() => { if (scroller) scroller.scrollTop = scroller.scrollHeight; });
    return () => cancelAnimationFrame(id);
  });
</script>

<div class="scroller" bind:this={scroller} onscroll={onScroll}>
  <div class="stack">
    {#if store.lastError}
      <div class="err" role="alert">{store.lastError}</div>
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
          <div class="typing" aria-label="对方正在输入"><span></span><span></span><span></span></div>
        </div>
      {/if}
    {/if}
  </div>
</div>

<style>
  .scroller { flex: 1; overflow-y: auto; background: transparent; }
  .stack { display: flex; flex-direction: column; gap: 14px; padding: 22px var(--gutter, 28px) 12px; max-width: var(--content-max); width: 100%; margin: 0 auto; }

  .err { align-self: center; color: var(--danger); font-size: 12px; background: color-mix(in srgb, var(--danger) 12%, transparent); border-radius: var(--radius-control); padding: 7px 12px; }

  .row { display: flex; gap: 10px; align-items: flex-start; }
  .typing { display: inline-flex; gap: 5px; padding: 13px 14px; background: var(--in-bubble); border-radius: var(--bubble-radius); border-top-left-radius: 3px; box-shadow: 0 1px 1.5px rgba(20,22,28,0.08); }
  .typing span { width: 6px; height: 6px; border-radius: 50%; background: var(--ink-faint); animation: bounce 1.2s infinite ease-in-out; }
  .typing span:nth-child(2) { animation-delay: 0.15s; }
  .typing span:nth-child(3) { animation-delay: 0.3s; }
  @keyframes bounce { 0%, 60%, 100% { transform: translateY(0); opacity: 0.4; } 30% { transform: translateY(-4px); opacity: 1; } }
</style>
