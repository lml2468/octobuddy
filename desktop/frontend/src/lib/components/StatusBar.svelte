<script lang="ts">
  // The live activity strip: a single compact line pinned just above the
  // composer, showing the agent's current step in the running turn — the latest
  // tool call or a thinking marker. It shows ONE record (the newest), and by
  // construction it carries no model prose: the store never routes answer text
  // here, so the final answer cannot leak into the strip. Cleared on the reply.
  import { store } from "../store.svelte";

  type Current = { kind: "tool" | "thinking"; text: string } | null;

  // Newest record = the last process step (tool / thinking). Null when idle.
  const current = $derived.by<Current>(() => {
    const p = store.currentSession?.proc;
    if (!p?.active) return null;
    const last = p.steps[p.steps.length - 1];
    return last ? { kind: last.kind, text: last.text } : { kind: "thinking", text: "Working…" };
  });
</script>

{#if current}
  <!-- No role="status" / aria-live: a tool-heavy turn fires N events,
       and SR users do not need every tool name read aloud. The pulse +
       aria-label below is the silent visual cue. -->
  <div class="strip">
    <div class="inner">
      <span class="pulse" class:tool={current.kind === "tool"} aria-label={current.kind === "tool" ? "正在调用工具" : "思考中"}></span>
      {#if current.kind === "tool"}
        <span class="label mono" title={current.text}>{current.text}</span>
      {:else}
        <span class="label muted">{current.text}</span>
      {/if}
    </div>
  </div>
{/if}

<style>
  .strip {
    background: color-mix(in srgb, var(--surface) 60%, transparent);
    backdrop-filter: blur(20px) saturate(160%); -webkit-backdrop-filter: blur(20px) saturate(160%);
    border-top: 1px solid var(--hairline);
    padding: 7px var(--gutter) 7px;
  }
  .inner {
    max-width: var(--content-max);
    margin: 0 auto;
    display: flex;
    align-items: center;
    gap: 9px;
    min-width: 0;
  }

  /* A soft breathing dot — the single "still working" affordance in this strip
     (the chat keeps the typing bubble). */
  .pulse {
    flex: 0 0 auto;
    width: 7px; height: 7px; border-radius: 50%;
    background: var(--accent);
    box-shadow: 0 0 0 0 color-mix(in srgb, var(--accent) 55%, transparent);
    animation: pulse 1.5s ease-in-out infinite;
  }
  .pulse.tool { border-radius: 2px; } /* square nib for a tool step */

  .label {
    min-width: 0; flex: 1 1 auto;
    font-size: 12px; line-height: 1.4; color: var(--ink-soft);
    white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
  }
  .label.mono { font-family: var(--mono); font-size: 11px; }
  .label.muted { color: var(--ink-faint); font-style: italic; }

  @keyframes pulse {
    0%   { box-shadow: 0 0 0 0 color-mix(in srgb, var(--accent) 55%, transparent); opacity: 1; }
    70%  { box-shadow: 0 0 0 6px color-mix(in srgb, var(--accent) 0%, transparent); opacity: 0.65; }
    100% { box-shadow: 0 0 0 0 color-mix(in srgb, var(--accent) 0%, transparent); opacity: 1; }
  }
  @media (prefers-reduced-motion: reduce) {
    .pulse { animation: none; }
  }
</style>
