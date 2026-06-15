<script lang="ts">
  import Octopus from "./Octopus.svelte";

  let {
    name = "",
    size = 40,
    octopus = false,
  }: { name?: string; size?: number; octopus?: boolean } = $props();

  // Curated, muted avatar palette (paired bg/fg) — deterministic per name.
  const palette = [
    "#3b7fe0", "#e07a3b", "#2faf73", "#a05fd6",
    "#d65f8f", "#3aa6b9", "#c79a2e", "#6b74d6",
  ];
  function hash(s: string): number {
    let h = 0;
    for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0;
    return Math.abs(h);
  }
  const color = $derived(palette[hash(name || "x") % palette.length]);
  const initial = $derived((name.trim()[0] || "?").toUpperCase());
  const radius = $derived(Math.round(size * 0.28));
</script>

<div
  class="avatar"
  style="width:{size}px;height:{size}px;border-radius:{radius}px;background:{octopus ? 'var(--accent)' : color};font-size:{Math.round(size * 0.42)}px"
>
  {#if octopus}
    <span class="oc" style="color:#fff"><Octopus size={Math.round(size * 0.62)} /></span>
  {:else}
    {initial}
  {/if}
</div>

<style>
  .avatar {
    display: inline-flex; align-items: center; justify-content: center;
    color: #fff; font-weight: 600; flex: 0 0 auto;
    user-select: none; overflow: hidden;
    box-shadow: inset 0 0 0 0.5px rgba(0, 0, 0, 0.06);
  }
  .oc { display: inline-flex; }
</style>
