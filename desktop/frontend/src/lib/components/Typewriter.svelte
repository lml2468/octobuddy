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
    if (shown < text.length) {
      // Only keep the rAF loop alive while we have characters left to reveal.
      // Without this guard, every finalized historical bubble keeps polling
      // requestAnimationFrame for the lifetime of the transcript — N bubbles
      // × 60 fps × zero work each. The text-change $effect below re-arms it
      // whenever new content arrives.
      raf = requestAnimationFrame(tick);
    } else {
      raf = 0;
    }
  }

  $effect(() => {
    // Reading text.length here registers the effect's dependency on the
    // prop so a fresh stream-delta restarts the loop. The cancel guard
    // prevents double-scheduling while a previous tick is still pending.
    void text.length;
    if (shown < text.length && raf === 0) {
      raf = requestAnimationFrame(tick);
    }
    return () => {
      if (raf !== 0) {
        cancelAnimationFrame(raf);
        raf = 0;
      }
    };
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
    background: var(--accent);
    opacity: 0.7;
    animation: blink 1s steps(2, start) infinite;
  }
  @keyframes blink { 50% { opacity: 0; } }
</style>
