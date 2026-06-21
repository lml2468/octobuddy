<script lang="ts">
  // ⌘K command palette — search Spaces (bots) + conversations and jump, or run
  // a command (open settings / skills / workflows / token usage). Wired to the
  // real store; no mock data. Fully keyboard-driven: ↑/↓ to move, Enter to pick,
  // Esc to close.
  import { store } from "../store.svelte";
  import { isImeComposing } from "../keys";
  import Avatar from "./Avatar.svelte";

  let { onclose, onedit, onskills, onworkflows, onusage }:
    { onclose: () => void; onedit: () => void; onskills: () => void; onworkflows: () => void; onusage: () => void } = $props();

  let q = $state("");
  let active = $state(0);
  let input: HTMLInputElement;
  let listEl: HTMLDivElement;

  // Focus the input one microtask after mount — Svelte needs a tick to commit
  // the bind:this. setTimeout returns a handle we clear in cleanup so a
  // double-⌘K that closes the palette within 40 ms doesn't pin a closure to
  // a detached node (round 12 F9).
  $effect(() => {
    const id = setTimeout(() => input?.focus(), 40);
    return () => clearTimeout(id);
  });

  const ql = $derived(q.trim().toLowerCase());
  const bots = $derived(store.bots.filter((b) => !ql || b.id.toLowerCase().includes(ql)));
  const sessions = $derived(
    store.sessions
      .filter((s) => !ql || s.title.toLowerCase().includes(ql) || (s.preview ?? "").toLowerCase().includes(ql))
      .slice(0, 8),
  );
  const commands = $derived(
    [
      { label: "编辑 Bot", run: onedit },
      { label: "技能", run: onskills },
      { label: "工作流", run: onworkflows },
      { label: "用量", run: onusage },
    ].filter((c) => !ql || c.label.toLowerCase().includes(ql)),
  );

  type Item =
    | { kind: "space"; label: string; sub: string; avatar: string; run: () => void }
    | { kind: "session"; label: string; sub: string; avatar: string; run: () => void }
    | { kind: "cmd"; label: string; run: () => void };

  // Flat, index-addressable list across all three sections for keyboard nav.
  const items = $derived<Item[]>([
    ...bots.map((b) => ({ kind: "space" as const, label: b.id, sub: b.connected ? "在线" : "离线", avatar: b.id, run: () => gotoBot(b.id) })),
    ...sessions.map((s) => ({ kind: "session" as const, label: s.title, sub: s.botId, avatar: s.title, run: () => gotoSession(s.botId, s.key) })),
    ...commands.map((c) => ({ kind: "cmd" as const, label: c.label, run: () => runCmd(c.run) })),
  ]);

  // Keep the highlight in range and reset to the top whenever the query changes.
  $effect(() => { ql; active = 0; });
  $effect(() => { if (active >= items.length) active = Math.max(0, items.length - 1); });
  $effect(() => {
    active;
    listEl?.querySelector<HTMLElement>(".pitem.active")?.scrollIntoView({ block: "nearest" });
  });

  function gotoBot(id: string) { store.selectBot(id); onclose(); }
  function gotoSession(botId: string, key: string) { if (store.selectedBotId !== botId) store.selectBot(botId); store.selectSession(key); onclose(); }
  function runCmd(fn: () => void) { onclose(); fn(); }

  function onKey(e: KeyboardEvent) {
    // Skip during IME composition. See lib/keys.ts isImeComposing — CJK
    // commit Enter would otherwise pick the highlighted palette item
    // mid-pinyin/kana entry.
    if (isImeComposing(e)) return;
    if (e.key === "Escape") { e.preventDefault(); onclose(); }
    else if (e.key === "ArrowDown") { e.preventDefault(); if (items.length) active = (active + 1) % items.length; }
    else if (e.key === "ArrowUp") { e.preventDefault(); if (items.length) active = (active - 1 + items.length) % items.length; }
    else if (e.key === "Enter") { e.preventDefault(); items[active]?.run(); }
  }
  // Section header shown before the first item of each kind.
  function headFor(i: number): string | null {
    const k = items[i].kind;
    if (i > 0 && items[i - 1].kind === k) return null;
    return k === "space" ? "Spaces" : k === "session" ? "会话" : "命令";
  }
</script>

<div class="scrim" onclick={onclose} role="presentation">
  <!-- svelte-ignore a11y_click_events_have_key_events (scrim handles keys; this onclick only stops propagation) -->
  <div class="palette" role="dialog" aria-modal="true" aria-label="命令面板" tabindex="-1" onclick={(e) => e.stopPropagation()}>
    <div class="pinput">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" style="color:var(--ink-faint)"><circle cx="11" cy="11" r="7"/><path d="m21 21-4.3-4.3"/></svg>
      <input bind:this={input} bind:value={q} onkeydown={onKey} placeholder="搜索会话、切换 Space 或执行命令…" aria-label="搜索" />
      <span class="esc">esc</span>
    </div>
    <div class="plist" bind:this={listEl}>
      {#each items as it, i (it.kind + it.label + i)}
        {#if headFor(i)}<div class="psec">{headFor(i)}</div>{/if}
        <button class="pitem" class:active={i === active} onclick={it.run} onmousemove={() => (active = i)}>
          {#if it.kind === "cmd"}
            <span class="cmd-ico"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="m9 18 6-6-6-6"/></svg></span>
          {:else}
            <Avatar name={it.avatar} size={24} />
          {/if}
          <span class="pl">{it.label}</span>
          {#if it.kind !== "cmd"}<span class="ph">{it.sub}</span>{/if}
        </button>
      {/each}
      {#if items.length === 0}
        <div class="psec">无匹配结果</div>
      {/if}
    </div>
  </div>
</div>

<style>
  .scrim { position: fixed; inset: 0; z-index: 200; background: color-mix(in srgb, var(--grad-b) 45%, transparent); backdrop-filter: blur(8px); -webkit-backdrop-filter: blur(8px); display: grid; place-items: start center; padding-top: 14vh; }
  .palette { width: min(580px, 90vw); background: var(--glass-active, var(--surface)); backdrop-filter: blur(40px) saturate(180%); -webkit-backdrop-filter: blur(40px) saturate(180%); border: 1px solid var(--glass-border); border-radius: 16px; box-shadow: 0 24px 60px rgba(0,0,0,0.22); overflow: hidden; }
  .pinput { display: flex; align-items: center; gap: 12px; padding: 16px 18px; border-bottom: 1px solid var(--border-soft, var(--hairline)); }
  .pinput svg { width: 20px; height: 20px; flex: 0 0 auto; }
  .pinput input { flex: 1; border: none; outline: none; background: none; font-size: 18px; color: var(--ink); }
  .pinput input::placeholder { color: var(--ink-faint); }
  .esc { font-family: var(--mono); font-size: 11px; color: var(--ink-faint); background: color-mix(in srgb, var(--ink) 6%, transparent); padding: 2px 7px; border-radius: 6px; }
  .plist { max-height: 340px; overflow-y: auto; padding: 8px; }
  .psec { font-size: 11px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.05em; color: var(--ink-faint); padding: 8px 10px 4px; }
  .pitem { display: flex; align-items: center; gap: 12px; width: 100%; text-align: left; padding: 9px 10px; border: none; background: transparent; border-radius: 8px; color: var(--ink); transition: background .12s ease; }
  .pitem:hover, .pitem.active { background: var(--accent-bg-hover); }
  .pitem.active { box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--accent) 35%, transparent); }
  .cmd-ico { width: 24px; height: 24px; flex: 0 0 24px; border-radius: 7px; display: grid; place-items: center; background: color-mix(in srgb, var(--accent) 12%, transparent); color: var(--accent-strong, var(--accent)); }
  .cmd-ico svg { width: 14px; height: 14px; }
  .pl { flex: 1; font-size: 14px; }
  .ph { font-family: var(--mono); font-size: 11px; color: var(--ink-faint); }
</style>
