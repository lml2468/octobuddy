<script lang="ts">
  import type { Message } from "../store.svelte";
  import { renderMarkdown } from "../markdown";
  import Typewriter from "./Typewriter.svelte";

  let { message }: { message: Message } = $props();

  const isUser = $derived(message.role === "user");
  const isTool = $derived(message.role === "tool");
  const html = $derived(!isUser && !isTool && !message.streaming ? renderMarkdown(message.text) : "");

  function copy() {
    navigator.clipboard?.writeText(message.text);
  }
</script>

{#if isTool}
  <div class="tool" title={message.text}>
    <span class="wrench">🔧</span> {message.text}
  </div>
{:else}
  <div class="row" class:user={isUser}>
    {#if !isUser}<div class="avatar bot" aria-hidden="true"></div>{/if}
    <div class="bubble" class:user={isUser} oncontextmenu={(e) => { e.preventDefault(); copy(); }} role="article">
      {#if isUser}
        <span class="plain">{message.text}</span>
      {:else if message.streaming}
        <Typewriter text={message.text} />
      {:else}
        <div class="md">{@html html}</div>
      {/if}
    </div>
    {#if isUser}<div class="avatar user" aria-hidden="true"></div>{/if}
  </div>
{/if}

<style>
  .row { display: flex; gap: 9px; align-items: flex-start; max-width: 100%; animation: bloom 0.45s cubic-bezier(0.2, 0.7, 0.2, 1) both; }
  .row.user { flex-direction: row-reverse; }

  /* Ink-bloom entrance: settle onto the page, slightly unfocused then sharp. */
  @keyframes bloom {
    from { opacity: 0; transform: translateY(6px); filter: blur(3px); }
    to   { opacity: 1; transform: translateY(0); filter: blur(0); }
  }
  @media (prefers-reduced-motion: reduce) { .row { animation: none; } }

  .avatar { width: 28px; height: 28px; border-radius: 50%; flex: 0 0 28px; box-shadow: var(--shadow-feather); }
  .avatar.bot { background: radial-gradient(circle at 38% 32%, color-mix(in srgb, var(--brand) 75%, var(--paper-raised)), var(--brand)); }
  .avatar.user { background: radial-gradient(circle at 38% 32%, color-mix(in srgb, var(--coral) 78%, var(--paper-raised)), var(--coral)); }

  .bubble {
    max-width: min(680px, 78%);
    padding: 9px 14px;
    border-radius: var(--radius);
    background: var(--paper-raised);
    border: 1px solid var(--hairline);
    box-shadow: var(--shadow-feather);
    line-height: 1.6;
  }
  .bubble.user {
    background: color-mix(in srgb, var(--coral) 24%, var(--paper-raised));
    border-color: color-mix(in srgb, var(--coral) 35%, var(--hairline));
  }
  .plain { white-space: pre-wrap; word-break: break-word; }

  .tool {
    align-self: center;
    margin: 2px auto;
    font-size: 11px;
    color: var(--ink-soft);
    background: color-mix(in srgb, var(--ink) 6%, transparent);
    padding: 3px 10px;
    border-radius: 999px;
    max-width: 80%;
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  }

  /* Markdown body */
  .md :global(p) { margin: 0 0 8px; }
  .md :global(p:last-child) { margin-bottom: 0; }
  .md :global(pre) {
    background: color-mix(in srgb, var(--ink) 7%, var(--paper-raised));
    border: 1px solid var(--hairline);
    border-radius: var(--radius-sm);
    padding: 10px 12px; overflow-x: auto;
    font-family: var(--mono); font-size: 12px;
  }
  .md :global(code) { font-family: var(--mono); font-size: 0.92em; }
  .md :global(:not(pre) > code) {
    background: color-mix(in srgb, var(--ink) 8%, transparent);
    padding: 1px 5px; border-radius: 5px;
  }
  .md :global(a) { color: var(--brand-strong); }
  .md :global(ul), .md :global(ol) { margin: 0 0 8px; padding-left: 20px; }
  .md :global(blockquote) { margin: 0 0 8px; padding-left: 12px; border-left: 3px solid var(--hairline); color: var(--ink-soft); }
</style>
