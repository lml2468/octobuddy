<script lang="ts">
  import Avatar from "./Avatar.svelte";

  let { onpick }: { onpick: (prompt: string) => void } = $props();

  const prompts = [
    "What can you help me with?",
    "Summarize the latest messages in this channel.",
    "Draft a concise status update for the team.",
  ];
</script>

<div class="empty">
  <Avatar octopus size={60} />
  <h1>Talk to your agent</h1>
  <p class="sub">Ask anything below, or start with one of these.</p>
  <div class="chips">
    {#each prompts as p}
      <button class="chip" onclick={() => onpick(p)}>{p}</button>
    {/each}
  </div>
</div>

<style>
  .empty {
    width: 100%; max-width: 400px; margin: 0 auto; min-height: 62vh;
    display: flex; flex-direction: column; align-items: center; justify-content: center;
    text-align: center; gap: 12px;
  }
  h1 { font-size: 22px; font-weight: 600; letter-spacing: -0.2px; margin: 6px 0 0; }
  .sub { color: var(--ink-soft); margin: 0; font-size: 13px; }
  .chips { display: flex; flex-direction: column; align-items: stretch; gap: 6px; width: 100%; margin-top: 8px; }
  .chip {
    width: 100%; text-align: left; padding: 10px 14px; font-size: 13px;
    background: color-mix(in srgb, var(--ink) 5%, transparent); color: var(--ink);
    border: none; border-radius: 10px;
    transition: background 0.14s ease;
  }
  .chip:hover { background: color-mix(in srgb, var(--accent) 14%, transparent); color: var(--accent-strong); }
</style>
