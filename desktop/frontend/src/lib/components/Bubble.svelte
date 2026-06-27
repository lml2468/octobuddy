<script lang="ts">
  import type { Message } from "../store.svelte";
  import { store } from "../store.svelte";
  import { renderMarkdown, onMarkdownCopyClick } from "../markdown";
  import Avatar from "./Avatar.svelte";
  import StepCard from "./StepCard.svelte";

  let { message, botId }: { message: Message; botId?: string } = $props();

  const isUser = $derived(message.role === "user");
  const isTool = $derived(message.role === "tool");
  const html = $derived(!isUser && !isTool ? renderMarkdown(message.text) : "");
  function fmtBytes(n: number): string {
    if (n < 1024) return `${n} B`;
    if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
    return `${(n / 1024 / 1024).toFixed(1)} MB`;
  }
 // senderLabel resolves the human author of a user-role bubble. Fallback
 // chain: the LIVE name from store.userNames (keyed on the authoring bot —
 // passed in as a prop, not read from global selection — plus senderUid;
 // this is what lets a bubble that first rendered with a bare uid converge
 // once the daemon's name.resolved event lands, AND what re-resolves a
 // reloaded row whose stored name was empty) → senderName (the name frozen
 // at append time) → senderUid (name still unknown) → "You" (Console
 // messages have neither). Reading store.userNames inside this $derived is
 // what subscribes the bubble to later map writes.
  const liveName = $derived(
    botId && message.senderUid ? store.userNames[botId]?.[message.senderUid] : undefined,
  );
  const senderLabel = $derived(liveName || message.senderName || message.senderUid || "You");
 // Show the sender name as a small label above the bubble ONLY when it
 // came from IM (senderName/senderUid present). Console-typed user
 // messages have neither and stay unlabeled — the operator knows they
 // typed it.
  const showSenderLabel = $derived(isUser && (message.senderName || message.senderUid));

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
      {#if isUser}<Avatar name={senderLabel} size={36} />{:else}<Avatar octopus size={36} />{/if}
    </span>
    <div class="bubble-col">
      {#if showSenderLabel}
        <div class="sender" title={message.senderUid || ""}>{senderLabel}</div>
      {/if}
      {#if !isUser && message.steps?.length}
 <!-- The process card that stayed attached after the turn: the tool calls /
             thinking this reply came from, all ✓. Sits above the answer bubble,
             sharing the column's width cap. Restored from history on reload. -->
        <StepCard steps={message.steps} />
      {/if}
      <div
        class="bubble"
        class:user={isUser}
      oncontextmenu={(e) => {
 // Hijack right-click ONLY when the user clicked outside any
 // interactive child (link, image, code block, table, form
 // control, …). A bare `e.preventDefault` on the bubble would
 // steal native context menus on agent-rendered links, leaving
 // no way to "open in new tab" or "copy link address".
 //
 // UL/OL/LI/BLOCKQUOTE/H1-6 are NOT in the bail list: those
 // elements are not interactive and have no native context-menu
 // value worth preserving, so including them
 // disabled copy-on-right-click for nearly every agent reply
 // (which almost always contains lists/headings). also
 // added FORM since an agent-emitted form's submit could navigate
 // the host page on a stray Enter — though markdown.ts now
 // FORBID_TAGS the whole form-control family anyway as the
 // primary defense.
        const t = e.target as HTMLElement | null;
        const limit = e.currentTarget as HTMLElement;
        const BAIL = /^(A|IMG|CODE|PRE|BUTTON|TABLE|TD|TH|SVG|DETAILS|SUMMARY|INPUT|SELECT|TEXTAREA|LABEL|FORM)$/;
        for (let n = t; n && n !== limit; n = n.parentElement) {
          if (BAIL.test(n.tagName)) return;
        }
        e.preventDefault();
        copy();
      }}
      role="article"
    >
      {#if copied}<span class="copied" aria-live="polite">已复制</span>{/if}
 <!-- Visible focusable copy button — keyboard users tab here, sighted
           users see it on hover. Right-click still works for muscle memory.
           replaces the prior `tabindex` on the bubble,
           which screen-readers announced as "article" with no actionable
           cue. Hidden until hover or focus to keep the chat surface clean. -->
      <button
        class="copy-btn"
        type="button"
        aria-label="复制消息"
        title="复制消息"
        onclick={copy}
      >
        <svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>
      </button>
      {#if message.cron}
        <!-- Corner badge for scheduler-fired prompts. Positioned outside
             the bubble's top-left so it reads as metadata (a stamp on
             the message) rather than content (a prefix that the user
             might mistake for part of what the agent saw). The bubble
             itself carries the plain prompt unchanged. -->
        <span class="cron-tag" aria-label="定时任务">cron</span>
      {/if}
      {#if isUser}
        {#if message.text}
          <span class="plain">{message.text}</span>
        {/if}
        {#if message.attachments && message.attachments.length}
          <div class="atts" aria-label="附件">
            {#each message.attachments as a}
              <span class="att" title="{a.name} · {fmtBytes(a.size)}">
                <span class="ic" aria-hidden="true">
                  {#if a.kind === "image"}
                    <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="3" width="18" height="18" rx="2"/><circle cx="9" cy="9" r="2"/><path d="m21 15-5-5L5 21"/></svg>
                  {:else}
                    <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/></svg>
                  {/if}
                </span>
                <span class="name">{a.name}</span>
              </span>
            {/each}
          </div>
        {/if}
      {:else}
        <div class="md" onclick={onMarkdownCopyClick} role="presentation">{@html html}</div>
      {/if}
      </div>
    </div>
  </div>
{/if}

<style>
  .row { display: flex; gap: 10px; align-items: flex-start; max-width: 100%; animation: rise 0.28s cubic-bezier(0.2, 0.7, 0.2, 1) both; }
  .row.user { flex-direction: row-reverse; }
  @keyframes rise { from { opacity: 0; transform: translateY(5px); } to { opacity: 1; transform: none; } }
  @media (prefers-reduced-motion: reduce) { .row { animation: none; } }

  .av { flex: 0 0 auto; margin-top: 1px; }

 /* bubble-col stacks the sender-name label (when shown) above the bubble.
    On user (row-reverse) rows the label aligns to the right edge so it
    sits over the bubble's leading corner. min-width: 0 lets the column
    shrink below its content's max-content width.

    The 74% width cap lives HERE, not on .bubble. A percentage max-width on
    .bubble would resolve against .bubble-col — but .bubble-col is itself a
    content-sized flex item (flex: 0 1 auto), so its width depends on the
    bubble: a circular constraint the browser collapses toward min-content,
    wrapping short messages to a sliver (e.g. "你能调用 gh cli 么?" to ~107px
    on two lines). The column's containing block is the .row, which IS a
    definite width, so capping the column resolves cleanly; the bubble then
    fits its content up to that cap. */
  .bubble-col { display: flex; flex-direction: column; gap: 3px; min-width: 0; max-width: 74%; align-items: flex-start; }
  .row.user .bubble-col { align-items: flex-end; }
  .sender {
    font-size: 11px;
    font-weight: 500;
    color: var(--ink-soft);
    padding: 0 4px;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    max-width: 240px;
  }

  .bubble {
    position: relative;
    max-width: 100%;
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
  .copy-btn {
    position: absolute; top: 5px; right: 5px; z-index: 1;
    width: 22px; height: 22px; padding: 0; border: none;
    display: inline-flex; align-items: center; justify-content: center;
    border-radius: 5px; cursor: pointer;
    color: var(--ink-soft);
    background: color-mix(in srgb, var(--ink) 8%, transparent);
 /* When hidden, pass clicks through to the markdown underneath — a link
       in the top-right corner of a long reply would otherwise be hijacked
       by the invisible button. Restored on hover /
       keyboard focus so the button stays clickable when visible. */
    opacity: 0; pointer-events: none;
    transition: opacity .14s ease, background .14s ease;
  }
  .bubble:hover .copy-btn,
  .copy-btn:focus-visible { opacity: 1; pointer-events: auto; }
  .copy-btn:hover { background: color-mix(in srgb, var(--ink) 16%, transparent); color: var(--ink); }
  .copy-btn:focus-visible { outline: 2px solid var(--accent); outline-offset: 1px; }
  .bubble.user .copy-btn {
    color: rgba(255, 255, 255, .85);
    background: rgba(255, 255, 255, .14);
  }
  .bubble.user .copy-btn:hover { background: rgba(255, 255, 255, .25); color: #fff; }
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
 /* Attachment chips inside a user bubble — small filename row showing
    what the operator sent alongside the text. Mirrors the Composer's
    pending-attachment chips so the visual story is consistent: chip
    appears in the Composer, chip lands in the bubble. */
  .atts { display: flex; flex-wrap: wrap; gap: 4px; margin-top: 6px; }
  .plain + .atts { margin-top: 8px; }
  .att {
    display: inline-flex; align-items: center; gap: 4px;
    padding: 2px 8px 2px 6px;
    background: rgba(255, 255, 255, 0.18);
    border-radius: 999px;
    font-size: 11px; line-height: 1.4;
    color: #fff; max-width: 200px;
  }
  .att .ic { display: inline-flex; opacity: 0.85; }
  .att .name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; min-width: 0; }
 /* "定时任务" corner badge — overlays the bubble's top-left corner,
    sticking outside by a few pixels so it reads as a stamp/postmark on
    the message rather than as content. Translucent green accent against
    the green user bubble; on light theme drops to a darker tone for
    contrast. position: absolute relies on the bubble being relative
    (it is — see .bubble's `position: relative` already). */
  .cron-tag {
    position: absolute;
    top: -8px;
    left: -8px;
    z-index: 2;
    font-family: var(--mono);
    font-size: 10px;
    font-weight: 600;
    line-height: 1;
    letter-spacing: 0.03em;
    padding: 3px 7px;
    border-radius: 999px;
    background: var(--accent);
    color: #fff;
    box-shadow: 0 1px 3px rgba(0, 0, 0, 0.18), 0 0 0 2px var(--surface);
    pointer-events: none;
    user-select: none;
  }
 /* On a USER bubble (right-aligned, row-reverse), the visual "top-left"
    of the bubble in DOM order is actually its top-right on screen because
    the row is reversed. Flip the badge horizontally so it lands at the
    visible top-left corner. */
  .row.user .cron-tag { left: auto; right: -8px; }

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
