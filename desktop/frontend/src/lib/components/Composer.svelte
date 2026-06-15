<script lang="ts">
  import { store } from "../store.svelte";

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
    if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); }
  }
</script>

<div class="composer">
  <div class="field">
    <textarea
      bind:this={ta}
      bind:value={draft}
      rows="1"
      placeholder="Message the agent…"
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
    background: var(--surface);
    padding: 12px var(--gutter) 14px;
  }
  .field {
    max-width: var(--content-max); margin: 0 auto;
    display: flex; align-items: flex-end; gap: 9px;
  }
  textarea {
    flex: 1; border: none; outline: none; resize: none;
    background: color-mix(in srgb, var(--ink) 6%, transparent);
    border-radius: 11px; padding: 10px 13px; line-height: 1.45; max-height: 140px;
    color: var(--ink); font-size: 14px;
  }
  textarea::placeholder { color: var(--ink-faint); }
  .send {
    flex: 0 0 34px; width: 34px; height: 34px; border-radius: 9px; border: none;
    display: grid; place-items: center; margin-bottom: 1px;
    background: color-mix(in srgb, var(--ink) 10%, transparent); color: var(--ink-faint);
    transition: background 0.15s ease, color 0.15s ease, transform 0.12s ease;
  }
  .send.on { background: var(--accent); color: #fff; }
  .send.on:hover { background: var(--accent-strong); transform: translateY(-1px); }
  .send:disabled { cursor: default; }
</style>
