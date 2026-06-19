<script lang="ts">
  // Shared header for the four settings screens (编辑 Bot / 技能 / 用量 / 工作流).
  // Fully reused — title + segmented nav + close — so the family stays identical.
  // `onnav` lets the host run an unsaved-changes guard before switching; screens
  // with extra header controls (e.g. the usage time-range) pass them via children.
  import type { Snippet } from "svelte";

  type Key = "editor" | "skills" | "usage" | "workflows";
  let { active, onclose, onnav = (fn: () => void) => fn(), onedit, onskills, onusage, onworkflows, children }:
    { active: Key; onclose: () => void; onnav?: (fn: () => void) => void;
      onedit?: () => void; onskills?: () => void; onusage?: () => void; onworkflows?: () => void;
      children?: Snippet } = $props();

  const tabs: { key: Key; label: string }[] = [
    { key: "editor", label: "编辑 Bot" },
    { key: "skills", label: "技能" },
    { key: "workflows", label: "工作流" },
    { key: "usage", label: "用量" },
  ];

  // Resolve the nav handler for a tab at click time (not captured at init), so
  // Svelte doesn't warn about referencing the prop's initial value.
  function navFor(key: Key): (() => void) | undefined {
    switch (key) {
      case "editor": return onedit;
      case "skills": return onskills;
      case "usage": return onusage;
      case "workflows": return onworkflows;
    }
  }
</script>

<header>
  <h2>设置</h2>
  <div class="seg" role="tablist" aria-label="设置分区">
    {#each tabs as t (t.key)}
      <button role="tab" aria-selected={t.key === active} class:on={t.key === active}
        onclick={() => { const fn = navFor(t.key); if (t.key !== active && fn) onnav(fn); }}>{t.label}</button>
    {/each}
  </div>
  <span class="hspacer"></span>
  {#if children}<span class="extra">{@render children()}</span>{/if}
  <button class="x" onclick={onclose} aria-label="关闭">✕</button>
</header>

<style>
  header { display: flex; align-items: center; gap: 12px; height: var(--header-h); padding: 0 18px 0 92px; -webkit-app-region: drag; border-bottom: 1px solid var(--border-soft, var(--hairline)); }
  .seg, .seg button, .extra, .x { -webkit-app-region: no-drag; }
  h2 { font-size: 17px; font-weight: 600; margin: 0; }
  .hspacer { flex: 1; }
  .extra { display: inline-flex; align-items: center; }
  .seg { display: inline-flex; background: color-mix(in srgb, var(--ink) 5%, transparent); border-radius: 10px; padding: 3px; }
  .seg button { padding: 6px 14px; border: none; background: transparent; border-radius: 8px; font-size: 13px; color: var(--ink-soft); cursor: pointer; transition: color .14s ease, background .14s ease; }
  .seg button.on { background: var(--surface); color: var(--ink); box-shadow: var(--elev-1, 0 1px 2px rgba(0,0,0,0.08)); }
  .seg button:not(.on):hover { color: var(--ink); }
  .x { width: 30px; height: 30px; display: grid; place-items: center; background: none; border: none; border-radius: 8px; color: var(--ink-soft); font-size: 15px; transition: background .14s ease, color .14s ease; }
  .x:hover { background: var(--ink-bg-hover); color: var(--ink); }
  .seg button:focus-visible, .x:focus-visible { outline: none; box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 30%, transparent); }
</style>
