<script lang="ts">
  import { store, type BotUsage } from "../store.svelte";
  import { onMount } from "svelte";

  let { onclose }: { onclose: () => void } = $props();

  // Range selector. `since` is Unix seconds at a LOCAL-midnight bound (0 = all
  // time), computed from the user's own calendar so "today" matches their tz.
  type RangeKey = "all" | "30d" | "7d" | "yesterday" | "today";
  const RANGES: { key: RangeKey; label: string }[] = [
    { key: "all", label: "All" },
    { key: "30d", label: "30 days" },
    { key: "7d", label: "7 days" },
    { key: "yesterday", label: "Yesterday" },
    { key: "today", label: "Today" },
  ];
  let range = $state<RangeKey>("all");

  function midnight(daysAgo: number): number {
    const d = new Date();
    d.setHours(0, 0, 0, 0);
    d.setDate(d.getDate() - daysAgo);
    return Math.floor(d.getTime() / 1000);
  }
  // since/until for a range key. until (today's midnight) is only used by the
  // single-day "yesterday" window; 0 elsewhere.
  function boundsFor(k: RangeKey): { since: number; until: number } {
    switch (k) {
      case "today": return { since: midnight(0), until: 0 };
      case "yesterday": return { since: midnight(1), until: midnight(0) };
      case "7d": return { since: midnight(7), until: 0 };
      case "30d": return { since: midnight(30), until: 0 };
      default: return { since: 0, until: 0 };
    }
  }
  const bounds = $derived(boundsFor(range));

  // Fetch the active range for every bot on open, and on each range change. We
  // drive fetches imperatively (onMount + the selector's onclick) rather than via
  // $effect, because loadUsage writes bot.usage — an effect that also read it
  // would re-run itself (update-depth loop). For "yesterday" we also need the
  // today-bound bucket to subtract, so fetch both.
  function fetchRange(k: RangeKey) {
    const bnd = boundsFor(k);
    store.loadUsage(bnd.since);
    if (k === "yesterday") store.loadUsage(bnd.until); // today's bucket, to subtract
  }
  onMount(() => fetchRange(range));
  function pickRange(k: RangeKey) {
    range = k;
    fetchRange(k);
  }

  const bots = $derived(store.bots);
  const ZERO: BotUsage = { inputTokens: 0, outputTokens: 0, cachedTokens: 0, cacheWriteTokens: 0, costUsd: 0, turns: 0 };
  // Usage for the active range. "Yesterday" is the today-bound minus the
  // today-bucket (until = today's midnight), derived from two `since` queries.
  function usageFor(b: { usage?: Record<number, BotUsage> }): BotUsage {
    const at = (s: number) => b.usage?.[s];
    if (range === "yesterday") {
      const a = at(bounds.since), today = at(bounds.until);
      if (!a) return ZERO;
      return today ? sub(a, today) : a;
    }
    return at(bounds.since) ?? ZERO;
  }
  function sub(a: BotUsage, b: BotUsage): BotUsage {
    return {
      inputTokens: a.inputTokens - b.inputTokens,
      outputTokens: a.outputTokens - b.outputTokens,
      cachedTokens: a.cachedTokens - b.cachedTokens,
      cacheWriteTokens: a.cacheWriteTokens - b.cacheWriteTokens,
      costUsd: a.costUsd - b.costUsd,
      turns: a.turns - b.turns,
    };
  }

  // Grand total across all bots for the active range.
  const total = $derived.by(() => {
    const t = { inputTokens: 0, outputTokens: 0, cachedTokens: 0, cacheWriteTokens: 0, costUsd: 0, turns: 0 };
    for (const b of bots) {
      const u = usageFor(b);
      t.inputTokens += u.inputTokens;
      t.outputTokens += u.outputTokens;
      t.cachedTokens += u.cachedTokens;
      t.cacheWriteTokens += u.cacheWriteTokens;
      t.costUsd += u.costUsd;
      t.turns += u.turns;
    }
    return t;
  });

  const anyUsage = $derived(total.turns > 0);

  // Compact number: 1_284_500 → "1.28M", 96_120 → "96.1K".
  function fmt(n: number): string {
    if (n < 1000) return String(n);
    if (n < 1_000_000) return `${(n / 1000).toFixed(1)}K`;
    if (n < 1_000_000_000) return `${(n / 1_000_000).toFixed(2)}M`;
    return `${(n / 1_000_000_000).toFixed(2)}B`;
  }
  function cost(n: number): string {
    return `$${n.toFixed(n < 1 ? 4 : 2)}`;
  }
</script>

<div class="scrim" onclick={onclose} role="presentation">
  <div class="modal" onclick={(e) => e.stopPropagation()} role="dialog" aria-label="Token usage">
    <header>
      <h2>Token Usage</h2>
      <div class="seg" role="tablist" aria-label="Date range">
        {#each RANGES as r (r.key)}
          <button role="tab" aria-selected={range === r.key} class:on={range === r.key} onclick={() => pickRange(r.key)}>{r.label}</button>
        {/each}
      </div>
      <button class="x" onclick={onclose} aria-label="Close">✕</button>
    </header>

    <div class="body">
      {#if bots.length === 0}
        <div class="empty">No bots configured.</div>
      {:else if !anyUsage}
        <div class="empty">No token usage in this range.</div>
      {:else}
        <!-- Grand total card -->
        <div class="total">
          <span class="tlabel">All bots</span>
          <div class="tstats">
            <div class="big"><span class="n">{fmt(total.inputTokens)}</span><span class="k">input</span></div>
            <div class="big"><span class="n">{fmt(total.outputTokens)}</span><span class="k">output</span></div>
            <div class="big"><span class="n">{fmt(total.cachedTokens)}</span><span class="k">cache read</span></div>
            <div class="big"><span class="n">{fmt(total.cacheWriteTokens)}</span><span class="k">cache write</span></div>
            <div class="big"><span class="n">{cost(total.costUsd)}</span><span class="k">cost</span></div>
          </div>
        </div>

        <!-- Per-bot table -->
        <div class="tablewrap">
          <table>
            <thead>
              <tr>
                <th class="lcol">Bot</th>
                <th>Input</th>
                <th>Output</th>
                <th>Cache R</th>
                <th>Cache W</th>
                <th>Cost</th>
                <th>Turns</th>
              </tr>
            </thead>
            <tbody>
              {#each bots as b (b.id)}
                {@const u = usageFor(b)}
                <tr>
                  <td class="lcol">
                    <span class="dot" class:on={b.connected}></span>{b.id}
                  </td>
                  <td class="num">{fmt(u.inputTokens)}</td>
                  <td class="num">{fmt(u.outputTokens)}</td>
                  <td class="num">{fmt(u.cachedTokens)}</td>
                  <td class="num">{fmt(u.cacheWriteTokens)}</td>
                  <td class="num">{cost(u.costUsd)}</td>
                  <td class="num dim">{u.turns}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>

        <p class="note">Per bot, persisted, bucketed by day. <strong>Cache R</strong> is input served (read) from the prompt cache; <strong>Cache W</strong> is input written into it. Cost is the agent-reported total. Usage recorded before per-day tracking appears only under <strong>All</strong>.</p>
      {/if}
    </div>
  </div>
</div>

<style>
  .scrim { position: fixed; inset: 0; z-index: 50; background: color-mix(in srgb, var(--ink) 28%, transparent); display: grid; place-items: center; }
  .modal { width: min(800px, 94vw); height: min(560px, 88vh); display: flex; flex-direction: column; background: var(--surface); border: 1px solid var(--hairline); border-radius: var(--radius); box-shadow: var(--shadow-pop); overflow: hidden; }
  header { display: flex; align-items: center; gap: 12px; padding: 14px 18px; border-bottom: 1px solid var(--hairline); }
  header h2 { font-size: 17px; flex: 1; }
  .x { background: none; border: none; color: var(--ink-soft); font-size: 15px; cursor: pointer; }

  /* Range selector */
  .seg { display: inline-flex; border: 1px solid var(--hairline); border-radius: 7px; overflow: hidden; }
  .seg button {
    font-size: 12px; padding: 5px 11px; border: none; background: transparent;
    color: var(--ink-soft); cursor: pointer; border-right: 1px solid var(--hairline);
    transition: background 0.12s ease, color 0.12s ease;
  }
  .seg button:last-child { border-right: none; }
  .seg button:hover { background: color-mix(in srgb, var(--ink) 6%, transparent); }
  .seg button.on { background: color-mix(in srgb, var(--accent) 16%, transparent); color: var(--accent-strong, var(--accent)); }

  .body { flex: 1; overflow-y: auto; padding: 18px; display: flex; flex-direction: column; gap: 16px; }
  .empty { color: var(--ink-faint); font-size: 13px; padding: 32px 8px; text-align: center; line-height: 1.5; }

  /* Grand-total card */
  .total {
    border: 1px solid var(--hairline); border-radius: 8px; padding: 16px 18px;
    background: color-mix(in srgb, var(--accent) 6%, var(--surface));
    display: flex; flex-direction: column; gap: 12px;
  }
  .tlabel { font-size: 12px; font-weight: 600; color: var(--accent-strong, var(--accent)); letter-spacing: 0.4px; text-transform: uppercase; }
  .tstats { display: grid; grid-template-columns: repeat(5, 1fr); gap: 12px; }
  .big { display: flex; flex-direction: column; gap: 2px; }
  .big .n { font-family: var(--mono); font-size: 20px; font-weight: 600; color: var(--ink); font-variant-numeric: tabular-nums; }
  .big .k { font-size: 11px; color: var(--ink-soft); }

  /* Per-bot table */
  .tablewrap { border: 1px solid var(--hairline); border-radius: 8px; overflow: hidden; }
  table { width: 100%; border-collapse: collapse; }
  th, td { text-align: right; padding: 9px 14px; font-size: 13px; }
  th { font-size: 11px; font-weight: 600; color: var(--ink-soft); text-transform: uppercase; letter-spacing: 0.3px; background: color-mix(in srgb, var(--ink) 3%, transparent); border-bottom: 1px solid var(--hairline); }
  tbody tr { border-bottom: 1px solid color-mix(in srgb, var(--hairline) 60%, transparent); }
  tbody tr:last-child { border-bottom: none; }
  tbody tr:hover { background: color-mix(in srgb, var(--ink) 3%, transparent); }
  .lcol { text-align: left; }
  td.lcol { font-size: 13px; color: var(--ink); display: flex; align-items: center; gap: 8px; }
  .num { font-family: var(--mono); color: var(--ink); font-variant-numeric: tabular-nums; }
  .num.dim { color: var(--ink-faint); }
  .dot { width: 7px; height: 7px; border-radius: 50%; background: var(--ink-faint); flex: 0 0 auto; }
  .dot.on { background: var(--accent); }

  .note { font-size: 11px; color: var(--ink-faint); line-height: 1.5; margin: 0; }
</style>
