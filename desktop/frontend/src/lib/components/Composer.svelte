<script lang="ts">
  import { store } from "../store.svelte";
  import { isImeComposing } from "../keys";

  let draft = $state("");
  let ta: HTMLTextAreaElement;

  const canSend = $derived(draft.trim().length > 0 && !!store.selectedBotId);

  export function setDraft(text: string) {
    draft = text;
    ta?.focus();
    autogrow();
  }
  function autogrow() {
    if (!ta) return;
    ta.style.height = "auto";
    ta.style.height = Math.min(ta.scrollHeight, 140) + "px";
  }
  function send() {
    if (!canSend) return;
    store.send(draft.trim());
    draft = "";
    requestAnimationFrame(autogrow);
  }
  function onKey(e: KeyboardEvent) {
    // Skip during IME composition (CJK Pinyin / Wubi / Kana / Hangul commit
    // candidates with Enter, delivered as keydown with isComposing=true).
    // Without this guard the handler swallows the commit and ships a
    // half-typed pinyin/romaji string — every other message for Chinese,
    // Japanese, Korean users.
    if (isImeComposing(e)) return;
    // ⌘↩ / Ctrl-↩ is the canonical "force send" — bypass the Shift-Enter
    // newline path and send even if a newline character is present
    // (matches Slack/Discord/Linear muscle memory; round 12 F14). Require
    // !shiftKey too so Shift+Cmd+Enter retains the prior insert-newline
    // behavior and doesn't silently force-send (round 13 F5).
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey) && !e.shiftKey) {
      e.preventDefault();
      send();
      return;
    }
    // Plain Enter sends; Shift / Alt / Option-Enter insert a newline.
    // Round 14 F #4: previously only Shift was excluded, so Alt+Enter
    // (VS Code "quick fix" muscle memory) and Option+Enter would silently
    // force-send a half-drafted message.
    if (e.key === "Enter" && !e.shiftKey && !e.altKey) { e.preventDefault(); send(); }
  }
</script>

<div class="composer">
  <div class="field">
    <textarea
      bind:this={ta}
      bind:value={draft}
      rows="1"
      placeholder="给 agent 发消息…"
      aria-label="消息"
      maxlength="32768"
      oninput={autogrow}
      onkeydown={onKey}
    ></textarea>
    <button class="send" class:on={canSend} onclick={send} disabled={!canSend} aria-label="Send">
      <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="M12 19V5M5 12l7-7 7 7"/></svg>
    </button>
  </div>
</div>

<style>
  .composer {
    background: color-mix(in srgb, var(--surface) 68%, transparent);
    backdrop-filter: blur(20px) saturate(160%); -webkit-backdrop-filter: blur(20px) saturate(160%);
    border-top: 1px solid var(--hairline);
    padding: 12px var(--gutter) 14px;
  }
  .field {
    max-width: var(--content-max); margin: 0 auto;
    display: flex; align-items: center; gap: 9px;
  }
  textarea {
    flex: 1; border: 1px solid var(--hairline); outline: none; resize: none;
    background: color-mix(in srgb, var(--ink) 6%, transparent);
    border-radius: var(--radius-control); padding: 10px 13px; line-height: 1.45; max-height: 140px;
    color: var(--ink); font-size: 14px;
    transition: border-color 0.15s ease, background 0.15s ease, box-shadow 0.15s ease;
  }
  textarea:focus {
    border-color: color-mix(in srgb, var(--accent) 65%, transparent);
    background: color-mix(in srgb, var(--ink) 3%, transparent);
    box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 16%, transparent);
  }
  textarea::placeholder { color: var(--ink-faint); }
  .send {
    flex: 0 0 34px; width: 34px; height: 34px; border-radius: var(--radius-control); border: none;
    display: grid; place-items: center;
    background: color-mix(in srgb, var(--ink) 10%, transparent); color: var(--ink-faint);
    transition: background 0.15s ease, color 0.15s ease, transform 0.12s ease, box-shadow 0.15s ease;
  }
  .send.on { background: var(--accent-grad); color: #fff; box-shadow: var(--accent-glow); }
  .send.on:hover { transform: translateY(-1px); box-shadow: 0 7px 20px color-mix(in srgb, var(--accent) 55%, transparent); }
  .send:disabled { cursor: default; }
  @media (prefers-reduced-motion: reduce) { .send.on:hover { transform: none; } }
</style>
