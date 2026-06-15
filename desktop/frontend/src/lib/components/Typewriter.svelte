<script lang="ts">
  // Smoothly reveals streamed text at a steady per-frame rate so bursty deltas
  // read as continuous typing (mirrors the Swift TypewriterText: advance a
  // fraction of the backlog each frame, min 1 char). Used for the in-flight
  // assistant bubble; the finalized message switches to rendered Markdown.
  let { text }: { text: string } = $props();

  let shown = $state(0);
  let raf = 0;

  function tick() {
    const total = text.length;
    if (shown < total) {
      const backlog = total - shown;
      shown = Math.min(total, shown + Math.max(1, Math.floor(backlog / 8)));
    }
    if (shown > total) shown = total; // text replaced (shorter) → snap
    raf = requestAnimationFrame(tick);
  }

  $effect(() => {
    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  });
</script>

<span class="tw">{text.slice(0, shown)}<span class="caret"></span></span>

<style>
  .tw { white-space: pre-wrap; word-break: break-word; }
  .caret {
    display: inline-block;
    width: 0.5em; height: 1em;
    margin-left: 1px;
    vertical-align: text-bottom;
    background: var(--brand);
    opacity: 0.7;
    animation: blink 1s steps(2, start) infinite;
  }
  @keyframes blink { 50% { opacity: 0; } }
</style>
