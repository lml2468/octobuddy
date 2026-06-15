<script lang="ts">
  import type { Message } from "../store.svelte";
  import { renderMarkdown } from "../markdown";
  import Avatar from "./Avatar.svelte";
  import Typewriter from "./Typewriter.svelte";

  let { message }: { message: Message } = $props();

  const isUser = $derived(message.role === "user");
  const isTool = $derived(message.role === "tool");
  const html = $derived(!isUser && !isTool && !message.streaming ? renderMarkdown(message.text) : "");

  function copy() { navigator.clipboard?.writeText(message.text); }

  // Delegated copy for code blocks rendered via {@html}.
  function onMdClick(e: MouseEvent) {
    const btn = (e.target as HTMLElement).closest(".cb-copy");
    if (!btn) return;
    const code = btn.closest(".codeblock")?.querySelector("code");
    if (code) {
      navigator.clipboard?.writeText(code.textContent ?? "");
      btn.textContent = "copied";
      setTimeout(() => (btn.textContent = "copy"), 1200);
    }
  }
</script>

{#if isTool}
  <div class="tool" title={message.text}>
    <span class="dot"></span>{message.text}
  </div>
{:else}
  <div class="row" class:user={isUser}>
    <span class="av">
      {#if isUser}<Avatar name="You" size={36} />{:else}<Avatar octopus size={36} />{/if}
    </span>
    <div class="bubble" class:user={isUser} oncontextmenu={(e) => { e.preventDefault(); copy(); }} role="article">
      {#if isUser}
        <span class="plain">{message.text}</span>
      {:else if message.streaming}
        <Typewriter text={message.text} />
      {:else}
        <div class="md" onclick={onMdClick} role="presentation">{@html html}</div>
      {/if}
    </div>
  </div>
{/if}

<style>
  .row { display: flex; gap: 10px; align-items: flex-start; max-width: 100%; animation: rise 0.28s cubic-bezier(0.2, 0.7, 0.2, 1) both; }
  .row.user { flex-direction: row-reverse; }
  @keyframes rise { from { opacity: 0; transform: translateY(5px); } to { opacity: 1; transform: none; } }
  @media (prefers-reduced-motion: reduce) { .row { animation: none; } }

  .av { flex: 0 0 auto; margin-top: 1px; }

  .bubble {
    max-width: 74%;
    padding: 9px 13px;
    font-size: 14px; line-height: 1.5;
    background: var(--in-bubble); color: var(--in-ink);
    border-radius: var(--bubble-radius);
    border-top-left-radius: 5px;
    box-shadow: 0 1px 1.5px rgba(20, 22, 28, 0.08);
  }
  .bubble.user {
    background: var(--out-bubble); color: var(--out-ink);
    border-top-left-radius: var(--bubble-radius);
    border-top-right-radius: 5px;
  }
  .plain { white-space: pre-wrap; word-break: break-word; }

  .tool {
    align-self: center; margin: 3px auto;
    display: inline-flex; align-items: center; gap: 6px;
    font-size: 11px; color: var(--ink-soft);
    background: color-mix(in srgb, var(--ink) 6%, transparent);
    padding: 4px 11px; border-radius: 999px;
    max-width: 80%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  }
  .tool .dot { width: 5px; height: 5px; border-radius: 50%; background: var(--accent); flex: 0 0 auto; }

  /* Markdown */
  .md :global(p) { margin: 0 0 7px; }
  .md :global(p:last-child) { margin-bottom: 0; }
  .md :global(code) { font-family: var(--mono); font-size: 0.88em; }
  .md :global(:not(pre) > code) { background: color-mix(in srgb, var(--ink) 8%, transparent); padding: 1px 5px; border-radius: 4px; }
  .md :global(a) { color: var(--accent-strong); }
  .md :global(ul), .md :global(ol) { margin: 0 0 7px; padding-left: 20px; }
  .md :global(blockquote) { margin: 0 0 7px; padding-left: 11px; border-left: 3px solid var(--hairline-strong); color: var(--ink-soft); }

  /* Code block — header bar (language + copy) over a tinted mono panel. */
  .md :global(.codeblock) {
    margin: 8px 0; border-radius: 7px; overflow: hidden;
    border: 1px solid var(--hairline);
    background: var(--code-bg);
  }
  .md :global(.cb-head) {
    display: flex; align-items: center; justify-content: space-between;
    padding: 5px 10px 5px 12px;
    border-bottom: 1px solid var(--hairline);
    background: color-mix(in srgb, var(--ink) 4%, transparent);
  }
  .md :global(.cb-lang) { font-family: var(--mono); font-size: 11px; color: var(--ink-soft); letter-spacing: 0.3px; }
  .md :global(.cb-copy) {
    font-family: var(--mono); font-size: 11px; color: var(--ink-faint);
    background: transparent; border: none; padding: 5px 9px; border-radius: 5px; cursor: pointer;
    transition: color 0.14s ease, background 0.14s ease;
  }
  .md :global(.cb-copy:hover) { color: var(--accent); background: color-mix(in srgb, var(--accent) 12%, transparent); }
  .md :global(.codeblock pre) { margin: 0; padding: 11px 13px; overflow-x: auto; }
  .md :global(.codeblock code) { font-family: var(--mono); font-size: 12px; line-height: 1.55; }
  .md :global(.tok-kw) { color: var(--tok-kw); }
  .md :global(.tok-str) { color: var(--tok-str); }
  .md :global(.tok-num) { color: var(--tok-num); }
  .md :global(.tok-com) { color: var(--ink-faint); font-style: italic; }
</style>
