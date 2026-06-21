<script lang="ts">
  // Small in-app confirm dialog (window.confirm is a no-op in the Wails webview).
  // Render conditionally; resolves via the two buttons. Esc=cancel, Enter=confirm.
  // On mount, focus moves to the primary button so keyboard handlers fire from
  // inside the dialog — otherwise the outer modal's keydown listener would eat
  // Escape and close the whole pane instead of just cancelling the confirm.
  //
  // The keydown listener is attached via addEventListener (not Svelte's
  // onkeydown attribute), because Svelte 5 DELEGATES keydown to the app root —
  // a delegated handler runs AFTER an ancestor's directly-attached listener has
  // already fired during bubble, so stopPropagation from there would be too
  // late to prevent the outer modal's Esc → close from firing.
  //
  // Tab is trapped INSIDE the confirm too — relying on the browser's native Tab
  // would hand focus to the outer modal's Tab trap, which cycles through the
  // whole form behind the confirm; a subsequent Esc then fires from a node
  // outside .cscrim and closes the modal instead of cancelling the confirm.
  import { onMount } from "svelte";
  let { message, confirmLabel = "确认", cancelLabel = "取消", danger = false, onresult }:
    { message: string; confirmLabel?: string; cancelLabel?: string; danger?: boolean; onresult: (ok: boolean) => void } = $props();

  let scrim: HTMLDivElement | undefined;
  let primary: HTMLButtonElement | undefined;

  function onKey(e: KeyboardEvent) {
    if (e.key === "Escape") { e.stopPropagation(); e.preventDefault(); onresult(false); return; }
    if (e.key === "Enter") { e.stopPropagation(); e.preventDefault(); onresult(true); return; }
    if (e.key === "Tab") {
      const btns = Array.from(scrim?.querySelectorAll<HTMLButtonElement>("button") ?? []);
      if (btns.length === 0) return;
      e.preventDefault();
      e.stopPropagation();
      const active = document.activeElement as HTMLElement | null;
      const idx = active ? btns.indexOf(active as HTMLButtonElement) : -1;
      const len = btns.length;
      const next = e.shiftKey
        ? (idx <= 0 ? len - 1 : idx - 1)
        : (idx < 0 || idx >= len - 1 ? 0 : idx + 1);
      btns[next].focus();
    }
  }

  onMount(() => {
    const opener = (document.activeElement as HTMLElement) ?? null;
    const node = scrim;
    node?.addEventListener("keydown", onKey);
    primary?.focus();
    return () => {
      node?.removeEventListener("keydown", onKey);
      try { opener?.focus?.(); } catch (_) {}
    };
  });
</script>

<div class="cscrim" bind:this={scrim} role="presentation" onclick={() => onresult(false)}>
  <!-- svelte-ignore a11y_click_events_have_key_events (keydown attached via addEventListener in onMount; onclick only stops propagation) -->
  <div class="confirm" role="alertdialog" aria-label="确认" aria-describedby="confirm-msg" tabindex="-1" onclick={(e) => e.stopPropagation()}>
    <p id="confirm-msg">{message}</p>
    <div class="cbtns">
      <button type="button" onclick={() => onresult(false)}>{cancelLabel}</button>
      <button type="button" bind:this={primary} class="primary" class:danger onclick={() => onresult(true)}>{confirmLabel}</button>
    </div>
  </div>
</div>

<style>
  .cscrim { position: absolute; inset: 0; z-index: 60; background: color-mix(in srgb, var(--ink) 32%, transparent); backdrop-filter: blur(2px); display: grid; place-items: center; }
  .confirm { width: min(360px, 80%); background: var(--surface); border: 1px solid var(--hairline); border-radius: var(--radius); box-shadow: var(--shadow-pop); padding: 18px; }
  .confirm p { margin: 0 0 14px; font-size: 13px; color: var(--ink); line-height: 1.5; }
  .cbtns { display: flex; justify-content: flex-end; gap: 8px; }
  .cbtns button { padding: 7px 14px; border-radius: var(--radius-control); border: 1px solid var(--hairline); background: color-mix(in srgb, var(--ink) 4%, var(--surface)); color: var(--ink); font-size: 12px; transition: background .14s ease, box-shadow .14s ease; }
  .cbtns button:focus-visible { outline: none; box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 30%, transparent); }
  .cbtns .primary { background: linear-gradient(135deg, var(--grad-a), var(--grad-b)); color: #fff; border: none; }
  .cbtns .primary.danger { background: var(--danger); }
</style>
