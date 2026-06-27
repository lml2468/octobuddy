<script lang="ts">
  import { store } from "../store.svelte";
  import Bubble from "./Bubble.svelte";
  import EmptyState from "./EmptyState.svelte";

  let { onpick }: { onpick: (prompt: string) => void } = $props();

  let scroller: HTMLDivElement;
  let atBottom = $state(true);

  const session = $derived(store.currentSession);
  const messages = $derived(session?.messages ?? []);
 // The in-flight turn is now an actual (pending) message in the list, so its
 // step/text changes must feed the auto-scroll tick. The pending placeholder is
 // always the last message while in flight, so check the tail (O(1)) rather
 // than scanning. Beyond message count + turn state, mix in the pending node's
 // step count, the DONE-step count (so a toolResult flipping running→done, which
 // doesn't change array length, still bumps), and the answer text length (so
 // streamed/whole text arrival re-pins the scroll).
  const last = $derived(messages[messages.length - 1]);
  const pending = $derived(last?.pending ? last : undefined);
  const tick = $derived(
    messages.length +
      (session?.awaiting ? 1 : 0) +
      (pending?.steps?.length ?? 0) +
      (pending?.steps?.reduce((n, st) => n + (st.status === "done" ? 1 : 0), 0) ?? 0) +
      (pending?.text?.length ?? 0),
  );

  function onScroll() {
    if (!scroller) return;
    atBottom = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 120;
  }
  $effect(() => {
    tick;
    if (!atBottom || !scroller) return;
 // Cancel any prior queued frame so a burst of new messages collapses
 // to one scroll; the cleanup also unpins the closure
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
      <div class="err" role="alert">
        <span>{store.lastError}</span>
        <button class="err-x" aria-label="关闭错误" title="关闭" onclick={() => store.clearLastError()}>×</button>
      </div>
    {/if}

    {#if messages.length === 0}
      <EmptyState {onpick} />
    {:else}
      <!-- The in-flight turn is a `pending` assistant message inside this list
           (Bubble renders the live step card + typing dots), so the live→final
           transition mutates one stable node instead of destroying a separate
           block and creating a new bubble — no redraw/flash. -->
      {#each messages as m (m.id)}
        <Bubble message={m} botId={session?.botId} />
      {/each}
    {/if}
  </div>
</div>

<style>
  .scroller { flex: 1; overflow-y: auto; background: transparent; }
  .stack { display: flex; flex-direction: column; gap: 14px; padding: 22px var(--gutter, 28px) 12px; max-width: var(--content-max); width: 100%; margin: 0 auto; }

  .err { align-self: center; color: var(--danger); font-size: 12px; background: color-mix(in srgb, var(--danger) 12%, transparent); border-radius: var(--radius-control); padding: 7px 12px; display: inline-flex; align-items: center; gap: 8px; }
  .err-x { border: 0; background: transparent; color: inherit; font-size: 16px; line-height: 1; padding: 0 2px; cursor: pointer; opacity: 0.7; }
  .err-x:hover { opacity: 1; }
</style>
