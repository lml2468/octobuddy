// Shared modal behaviour for the full-window settings panes and overlays:
// - Escape closes (calls opts.onclose)
// - marks the node aria-modal and gives it a focus target
// - moves focus into the dialog on mount, restores it to the opener on destroy
// - traps Tab focus inside the dialog (keyboard users can't tab out into the
// inert background)
//
// Usage: <div class="modal" use:modal={{ onclose }}> … </div>
// window.confirm is a no-op in the Wails webview, so closing is always routed
// through the supplied onclose (which may itself run an in-app confirm first).

type ModalOpts = { onclose: () => void };

const FOCUSABLE = 'input, textarea, button, [href], select, [tabindex]:not([tabindex="-1"])';

export function modal(node: HTMLElement, opts: ModalOpts) {
  let current = opts;
  const opener = (document.activeElement as HTMLElement) ?? null;

  node.setAttribute("aria-modal", "true");
  if (!node.getAttribute("role")) node.setAttribute("role", "dialog");

  function onKey(e: KeyboardEvent) {
    if (e.key === "Escape") {
      e.stopPropagation();
      current.onclose();
      return;
    }
    if (e.key === "Tab") {
 // Trap focus: cycle within the dialog's focusable controls so Tab/Shift+Tab
 // can't move focus into the inert background behind the modal.
 //
 // stopPropagation regardless of whether THIS modal handles the wrap
 // — when a wizard is mounted inside SettingsModal, BOTH `use:modal`
 // listeners fire on bubble. The outer modal's `querySelectorAll`
 // scopes to its own subtree which INCLUDES the inner modal's
 // controls, so the outer's `last` resolves to a node inside the
 // inner modal; on Shift+Tab from the inner's first input, the outer
 // sees `active === last` and yanks focus to its own first item
 // (the sidebar's first bot row), escaping the wizard. Stopping
 // propagation in the inner handler keeps the outer blind to the
 // Tab and preserves the inner's own trap.
      e.stopPropagation();
      const items = Array.from(node.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(
        (el) => !el.hasAttribute("disabled") && el.offsetParent !== null,
      );
      if (items.length === 0) {
        e.preventDefault();
        return;
      }
      const first = items[0];
      const last = items[items.length - 1];
      const active = document.activeElement as HTMLElement | null;
      if (e.shiftKey && active === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && active === last) {
        e.preventDefault();
        first.focus();
      }
    }
  }
  node.addEventListener("keydown", onKey);

 // Focus the first natural control, else the dialog itself.
 // queueMicrotask (was: setTimeout 40ms) so focus lands within the same
 // turn the modal mounts — a fast Tab keypress during the 40ms window
 // previously landed on a button behind the modal.
  const focusable = node.querySelector<HTMLElement>(FOCUSABLE);
  if (focusable) {
    queueMicrotask(() => focusable.focus());
  } else {
    node.tabIndex = -1;
    queueMicrotask(() => node.focus());
  }

  return {
    update(next: ModalOpts) {
      current = next;
    },
    destroy() {
      node.removeEventListener("keydown", onKey);
      try {
        opener?.focus?.();
      } catch (_) {}
    },
  };
}
