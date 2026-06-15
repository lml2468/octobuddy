<script lang="ts">
  import Octopus from "./Octopus.svelte";

  let { onpick }: { onpick: (prompt: string) => void } = $props();

  const prompts = [
    "What can you help me with?",
    "Summarize the latest messages in this channel.",
    "Draft a concise status update for the team.",
  ];
</script>

<div class="empty">
  <div class="hero">
    <div class="halo"></div>
    <Octopus size={60} />
  </div>
  <h1>Talk to your agent</h1>
  <p class="sub">Ask anything below, or start with one of these.</p>
  <div class="chips">
    {#each prompts as p}
      <button class="chip" onclick={() => onpick(p)}>
        <span class="spark">✦</span>
        <span class="label">{p}</span>
      </button>
    {/each}
  </div>
</div>

<style>
  .empty {
    width: 100%;
    max-width: 460px;
    margin: 0 auto;
    display: flex;
    flex-direction: column;
    align-items: center;
    text-align: center;
    padding-top: 8vh;
    gap: 14px;
  }
  .hero { position: relative; color: var(--brand); display: grid; place-items: center; width: 100px; height: 100px; }
  .halo {
    position: absolute; width: 160px; height: 160px; border-radius: 50%;
    background: radial-gradient(circle, color-mix(in srgb, var(--brand) 20%, transparent), transparent 70%);
    filter: blur(6px);
  }
  h1 { font-size: 30px; }
  .sub { color: var(--ink-soft); margin: 0; }

  .chips {
    width: 100%;
    display: flex;
    flex-direction: column;
    align-items: stretch;
    gap: 9px;
    margin-top: 4px;
  }
  .chip {
    display: flex;
    align-items: center;
    gap: 9px;
    width: 100%;
    text-align: left;
    padding: 11px 16px;
    background: var(--paper-raised);
    color: var(--ink);
    border: 1px solid var(--hairline);
    border-radius: 14px;
    box-shadow: var(--shadow-feather);
    transition: transform 0.15s ease, border-color 0.15s ease;
  }
  .chip:hover { transform: translateY(-1px); border-color: color-mix(in srgb, var(--brand) 40%, var(--hairline)); }
  .spark { color: var(--brand); flex: 0 0 auto; }
  .label { flex: 1; white-space: normal; }
</style>
