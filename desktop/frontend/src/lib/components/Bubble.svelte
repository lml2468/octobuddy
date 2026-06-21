<script lang="ts">
  import type { Message } from "../store.svelte";
  import { renderMarkdown, onMarkdownCopyClick } from "../markdown";
  import Avatar from "./Avatar.svelte";
  import Typewriter from "./Typewriter.svelte";

  let { message }: { message: Message } = $props();

  const isUser = $derived(message.role === "user");
  const isTool = $derived(message.role === "tool");
  const html = $derived(!isUser && !isTool && !message.streaming ? renderMarkdown(message.text) : "");

  let copied = $state(false);
  let copyTimer: ReturnType<typeof setTimeout> | undefined;
  function copy() {
    navigator.clipboard?.writeText(message.text);
    copied = true;
    clearTimeout(copyTimer);
    copyTimer = setTimeout(() => (copied = false), 1200);
  }
  // Clear the copy-confirmation timer on unmount. Without this, switching
  // sessions / resetting the transcript within the 1200 ms window
  // unmounts the Bubble but the setTimeout still fires, writing to a
  // detached component's reactive state and pinning the (now-detached)
  // closure in memory until the timer expires. Latent reactive-write leak.
  $effect(() => () => clearTimeout(copyTimer));
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
    <!-- svelte-ignore a11y_no_noninteractive_tabindex, a11y_no_noninteractive_element_interactions -->
    <div
      class="bubble"
      class:user={isUser}
      oncontextmenu={(e) => { e.preventDefault(); copy(); }}
      onkeydown={(e) => {
        // ⌘C / Ctrl-C when the bubble is keyboard-focused copies the message
        // text (round 12 F6 — right-click is the only mouse affordance and
        // would otherwise leave keyboard-only users with no way to copy a
        // reply). Don't intercept when the user has a real text selection
        // inside the bubble — let the browser's native copy win.
        if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "c"
            && !window.getSelection()?.toString()) {
          e.preventDefault();
          copy();
        }
      }}
      role="article"
      tabindex="0"
    >
      {#if copied}<span class="copied" aria-live="polite">已复制</span>{/if}
      {#if isUser}
        <span class="plain">{message.text}</span>
      {:else if message.streaming}
        <Typewriter text={message.text} />
      {:else}
        <div class="md" onclick={onMarkdownCopyClick} role="presentation">{@html html}</div>
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
    position: relative;
    max-width: 74%;
    padding: 9px 13px;
    font-size: 14px; line-height: 1.5;
    background: var(--in-bubble); color: var(--in-ink);
    border-radius: var(--bubble-radius);
    border-top-left-radius: 3px;
    border: 1px solid var(--bubble-border);
    box-shadow: var(--elev-1);
  }
  .copied {
    position: absolute; top: -10px; right: 8px; z-index: 2;
    font-size: 10px; font-weight: 600; color: #fff;
    background: color-mix(in srgb, var(--ink) 82%, transparent);
    padding: 2px 8px; border-radius: 999px; box-shadow: var(--elev-1);
    animation: copied-in 0.14s ease both;
  }
  @keyframes copied-in { from { opacity: 0; transform: translateY(3px); } to { opacity: 1; transform: none; } }
  .bubble.user {
    background: linear-gradient(135deg, var(--grad-a), var(--grad-b)); color: #fff;
    border: none;
    border-top-left-radius: var(--bubble-radius);
    border-top-right-radius: 3px;
    box-shadow: 0 6px 18px color-mix(in srgb, var(--grad-a) 38%, transparent), var(--elev-1);
    text-shadow: 0 1px 2px rgba(0, 0, 0, 0.14);
  }
  .bubble.user .plain { color: #fff; }
  .plain { white-space: pre-wrap; word-break: break-word; }

  .tool {
    align-self: center; margin: 3px auto;
    display: inline-flex; align-items: center; gap: 6px;
    font-size: 11px; color: var(--ink-soft);
    background: color-mix(in srgb, var(--ink) 6%, transparent);
    padding: 4px 11px; border-radius: 4px;
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
  /* Code-block + syntax-token rules are shared via lib/styles/markdown.css. */
</style>
