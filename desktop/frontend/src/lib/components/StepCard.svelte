<script lang="ts">
 // StepCard renders the agent's process steps for one turn as a compact card —
 // a list of tool calls / thinking markers, each with a ✓ (done) or an animated
 // ◌ (running). Used two ways: LIVE in the Transcript while a turn is in flight
 // (steps stream in, the last one spins), and ATTACHED above an assistant bubble
 // after the turn (all steps done, persisted across reloads). Renders nothing
 // for an empty list so a step-less reply looks identical to before.
 //
 // Each tool step shows a human-readable summary (the tool input's
 // "description") by default; a step that carries `detail` (the raw
 // Name(params)) is clickable to disclose it. Thinking steps and detail-less
 // tool steps render as plain, non-interactive rows.
  import type { ProcStep } from "../store.svelte";

  let { steps, live = false }: { steps: ProcStep[]; live?: boolean } = $props();

 // Per-row expanded state, keyed by step id (stable per render). Component-local:
 // the live card remounts each turn and attached cards are per-bubble, so no
 // cross-card leakage.
  let expanded = $state(new Set<string>());
  const toggle = (id: string) => {
    // Reassign so Svelte's $state tracks the mutation.
    const next = new Set(expanded);
    next.has(id) ? next.delete(id) : next.add(id);
    expanded = next;
  };
  const canExpand = (s: ProcStep) => s.kind === "tool" && !!s.detail;
</script>

{#snippet icon(step: ProcStep)}
  {#if step.status === "done"}
    <svg class="ic ok" viewBox="0 0 16 16" width="14" height="14" aria-hidden="true">
      <path d="M3.5 8.5l3 3 6-7" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/>
    </svg>
  {:else}
    <span class="ic spin" aria-label="进行中"></span>
  {/if}
{/snippet}

{#if steps.length}
  <div class="card" class:live aria-label="处理步骤">
    {#each steps as step (step.id)}
      {#if canExpand(step)}
        <div class="step-wrap">
          <button
            type="button"
            class="step row-btn"
            aria-expanded={expanded.has(step.id)}
            onclick={() => toggle(step.id)}
          >
            {@render icon(step)}
            <span class="text" title={step.detail}>{step.text}</span>
            <svg class="chev" class:open={expanded.has(step.id)} viewBox="0 0 16 16" width="12" height="12" aria-hidden="true">
              <path d="M4 6l4 4 4-4" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>
            </svg>
          </button>
          {#if expanded.has(step.id)}
            <pre class="detail">{step.detail}</pre>
          {/if}
        </div>
      {:else}
        <div class="step" class:running={step.status === "running"}>
          {@render icon(step)}
          <span class="text" class:thinking={step.kind === "thinking"} title={step.text}>{step.text}</span>
        </div>
      {/if}
    {/each}
  </div>
{/if}

<style>
 /* The card sits left-aligned, sharing the bubble column's width cap when
    attached. A tinted surface + leading accent rail reads as "process", set
    apart from the answer bubble below it. */
  .card {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 10px 12px;
    background: color-mix(in srgb, var(--ink) 4%, var(--surface));
    border: 1px solid var(--hairline);
    border-left: 2px solid color-mix(in srgb, var(--accent) 55%, transparent);
    border-radius: 8px;
    box-shadow: var(--elev-1);
  }

  .step-wrap { display: flex; flex-direction: column; gap: 4px; min-width: 0; }
  .step { display: flex; align-items: center; gap: 8px; min-width: 0; }

 /* An expandable row is a full-width reset button — left-aligned, no chrome,
    a pointer cursor — so the whole line is the disclosure target. */
  .row-btn {
    width: 100%;
    border: 0;
    background: none;
    padding: 0;
    font: inherit;
    color: inherit;
    text-align: left;
    cursor: pointer;
  }
  .row-btn:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; border-radius: 4px; }

  .ic { flex: 0 0 auto; width: 14px; height: 14px; }
  .ic.ok { color: var(--success); }

 /* Running spinner — a small ring that rotates. The accent-tinted track with
    an accent top arc reads as "in progress". */
  .spin {
    display: inline-block;
    width: 12px; height: 12px;
    border-radius: 50%;
    border: 2px solid color-mix(in srgb, var(--accent) 25%, transparent);
    border-top-color: var(--accent);
    animation: spin 0.7s linear infinite;
  }

  .text {
    min-width: 0; flex: 1 1 auto;
    font-family: var(--mono);
    font-size: 12px; line-height: 1.4;
    color: var(--ink-soft);
    white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
  }
 /* Thinking markers aren't code — render them as soft italic prose, not mono. */
  .text.thinking { font-family: inherit; font-style: italic; color: var(--ink-faint); }

 /* Disclosure chevron at the row's trailing edge; rotates when open. */
  .chev { flex: 0 0 auto; color: var(--ink-faint); transition: transform 0.15s ease; }
  .chev.open { transform: rotate(180deg); }

 /* Expanded raw Name(params): mono, wrapped, indented under the summary, set
    off by a hairline. */
  .detail {
    margin: 0 0 0 22px;
    padding: 6px 8px;
    border-left: 2px solid var(--hairline);
    font-family: var(--mono);
    font-size: 11px; line-height: 1.45;
    color: var(--ink-soft);
    white-space: pre-wrap;
    word-break: break-word;
  }

  @keyframes spin { to { transform: rotate(360deg); } }
  @media (prefers-reduced-motion: reduce) {
    .spin { animation: none; }
    .chev { transition: none; }
  }
</style>
