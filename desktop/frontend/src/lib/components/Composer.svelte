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
    ta.style.height = Math.min(ta.scrollHeight, 150) + "px";
  }

  function send() {
    if (!canSend) return;
    store.send(draft.trim());
    draft = "";
    requestAnimationFrame(autogrow);
  }

  function onKey(e: KeyboardEvent) {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  }
</script>

<div class="composer">
  <div class="pill">
    <textarea
      bind:this={ta}
      bind:value={draft}
      rows="1"
      placeholder="Message the agent…"
      oninput={autogrow}
      onkeydown={onKey}
    ></textarea>
    <button class="send" class:on={canSend} onclick={send} disabled={!canSend} aria-label="Send">
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="M12 19V5M5 12l7-7 7 7"/></svg>
    </button>
  </div>
</div>

<style>
  .composer { padding: 8px 16px 16px; }
  .pill {
    display: flex; align-items: flex-end; gap: 8px;
    background: var(--paper-raised);
    border: 1px solid var(--hairline);
    border-radius: 22px;
    padding: 6px 6px 6px 16px;
    box-shadow: var(--shadow-feather);
    transition: border-color 0.18s ease, box-shadow 0.18s ease;
  }
  .pill:focus-within { border-color: color-mix(in srgb, var(--brand) 55%, var(--hairline)); }
  textarea {
    flex: 1; border: none; outline: none; background: transparent; resize: none;
    line-height: 1.5; padding: 7px 0; max-height: 150px; color: var(--ink);
  }
  textarea::placeholder { color: var(--ink-faint); }
  .send {
    flex: 0 0 36px; width: 36px; height: 36px; border-radius: 50%;
    border: none; display: grid; place-items: center;
    background: color-mix(in srgb, var(--ink) 12%, transparent); color: var(--paper-raised);
    transition: transform 0.15s ease, background 0.15s ease;
  }
  .send.on { background: var(--brand); color: #fff; }
  .send.on:hover { transform: scale(1.08); }
  .send:disabled { cursor: default; }
</style>
