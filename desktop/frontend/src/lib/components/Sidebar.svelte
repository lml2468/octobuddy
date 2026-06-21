<script lang="ts">
  // Arc sidebar-first navigation — merges the old dark bot-rail and the
  // conversation list into one translucent panel: brand, ⌘K command-bar,
  // Spaces (bots), conversation tabs, and a settings footer. The window's
  // per-Space gradient blooms behind it.
  import { onMount } from "svelte";
  import { store } from "../store.svelte";
  import Avatar from "./Avatar.svelte";
  import XMark from "./XMark.svelte";

  let { onedit, onusage, onpalette, collapsed = false }:
    { onedit: () => void; onusage: () => void; onpalette: () => void; collapsed?: boolean } = $props();

  function relTime(ms: number): string {
    if (!ms) return "";
    const d = (Date.now() - ms) / 1000;
    if (d < 60) return "now";
    if (d < 3600) return `${Math.floor(d / 60)}m`;
    if (d < 86400) return `${Math.floor(d / 3600)}h`;
    return `${Math.floor(d / 86400)}d`;
  }
  const preview = (s: any) =>
    s.awaiting ? "replying…" : (s.messages.at(-1)?.text ?? s.preview ?? "No messages yet");

  // In-app light/dark toggle (persisted). The store also honours ?theme=.
  // The button shows the icon for the OPPOSITE state — sun while in dark
  // (click → light), moon while in light (click → dark) — the standard
  // affordance hint for what the click will do.
  function currentTheme(): "dark" | "light" {
    const cur = document.documentElement.getAttribute("data-theme");
    if (cur === "dark" || cur === "light") return cur;
    return matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  }
  // Read once at mount. Was `$effect(...)` but that registered zero
  // reactive deps (currentTheme reads document.documentElement, not state)
  // so it ran exactly once anyway — onMount is the honest version.
  let theme = $state<"dark" | "light">("dark");
  onMount(() => { theme = currentTheme(); });
  function toggleTheme() {
    const next = theme === "dark" ? "light" : "dark";
    document.documentElement.setAttribute("data-theme", next);
    theme = next;
    try { localStorage.setItem("xclaw:theme", next); } catch (_) {}
  }
</script>

<aside class="sidebar" class:collapsed>
  <div class="top" style="--wails-draggable: drag;">
    <span class="brand-mark" aria-hidden="true"><XMark size={17} /></span>
    <span class="brand-name">XClaw</span>
    <span class="spacer"></span>
    <button class="iconbtn theme-btn" style="--wails-draggable: no-drag;" title="切换明暗" aria-label="切换明暗" onclick={toggleTheme}>
      {#if theme === "dark"}
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg>
      {:else}
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/></svg>
      {/if}
    </button>
  </div>

  <button class="cmd-bar" onclick={onpalette} aria-label="命令面板" style="--wails-draggable: no-drag;">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><circle cx="11" cy="11" r="7"/><path d="m21 21-4.3-4.3"/></svg>
    <span class="t">搜索 · 命令 · 跳转</span><span class="kbd">⌘K</span>
  </button>

  <div class="lbl">BOTS</div>
  <div class="spaces">
    {#each store.bots as b (b.id)}
      <button class="space" class:sel={b.id === store.selectedBotId}
        title={b.id + (b.connected ? " · connected" : " · offline")}
        onclick={() => store.selectBot(b.id)}>
        <span class="tile"><Avatar name={b.id} size={26} /><span class="status" class:on={b.connected}></span></span>
        <span class="nm">{b.id}</span>
      </button>
    {/each}
  </div>

  <div class="lbl">会话</div>
  <div class="tabs">
    {#if !store.currentBot}
      <div class="empty">选择一个 Space</div>
    {:else if store.botSessions.length === 0}
      <div class="empty">还没有会话 — 打个招呼吧</div>
    {:else}
      {#each store.botSessions as s (s.key)}
        <button class="tab" class:sel={s.key === store.selectedKey} onclick={() => store.selectSession(s.key)}>
          <Avatar name={s.title} size={22} />
          <span class="b">
            <span class="r1"><span class="name">{s.title}</span><span class="time">{relTime(s.lastActivity)}</span></span>
            <span class="sub" class:replying={s.awaiting}>{preview(s)}</span>
          </span>
        </button>
      {/each}
    {/if}
  </div>

  <div class="foot">
    <button class="fbtn" onclick={onusage} title="Token 用量">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 3v18h18"/><rect x="7" y="13" width="3" height="5"/><rect x="12" y="9" width="3" height="9"/><rect x="17" y="5" width="3" height="13"/></svg>
      <span class="t">用量</span>
    </button>
    <button class="fbtn" onclick={onedit} title="设置">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 8 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H2a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 3.6 8a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H8a1.65 1.65 0 0 0 1-1.51V2a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V8a1.65 1.65 0 0 0 1.51 1H22a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>
      <span class="t">设置</span>
    </button>
  </div>
</aside>

<style>
  .sidebar {
    width: 264px; flex: 0 0 264px; height: 100%;
    display: flex; flex-direction: column;
    background: var(--glass-soft);
    backdrop-filter: blur(28px) saturate(160%); -webkit-backdrop-filter: blur(28px) saturate(160%);
    padding: 0 12px 12px; overflow: hidden;
    transition: width var(--motion-base, .32s) var(--ease-standard, ease), flex-basis var(--motion-base, .32s) var(--ease-standard, ease);
  }
  /* Collapsed: fully hidden — the chat-header toggle brings it back. */
  .sidebar.collapsed { width: 0; flex-basis: 0; min-width: 0; padding: 0; overflow: hidden; }

  .top { height: var(--header-h); flex: 0 0 var(--header-h); display: flex; align-items: center; gap: 8px; padding: 0 4px 0 64px; }
  /* Collapsed: drop the brand + theme toggle, leave only the expand control centered. */
  .sidebar.collapsed .top { padding: 0; justify-content: center; }
  .brand-mark { width: 30px; height: 30px; flex: 0 0 30px; border-radius: 8px; display: grid; place-items: center; color: #fff; background: linear-gradient(135deg, var(--grad-a), var(--grad-b)); box-shadow: 0 4px 14px color-mix(in srgb, var(--grad-a) 45%, transparent); }
  .brand-name { font-weight: 600; font-size: 15px; color: var(--ink); }
  .spacer { flex: 1; }
  .iconbtn { width: 30px; height: 30px; border-radius: 8px; border: none; background: transparent; color: var(--ink-soft); display: grid; place-items: center; transition: background .14s ease, color .14s ease, transform .12s ease; }
  .iconbtn svg { width: 17px; height: 17px; }
  .iconbtn:hover { background: var(--ink-bg-hover); color: var(--ink); }
  .iconbtn:active { transform: scale(0.9); }
  /* Collapsed: just the brand mark, centered (toggle lives in the chat header). */
  .sidebar.collapsed .theme-btn { display: none; }
  .sidebar.collapsed .brand-name, .sidebar.collapsed .lbl, .sidebar.collapsed .nm,
  .sidebar.collapsed .b, .sidebar.collapsed .cmd-bar .t, .sidebar.collapsed .cmd-bar .kbd,
  .sidebar.collapsed .foot .t { display: none; }
  @media (prefers-reduced-motion: reduce) { .iconbtn:active { transform: none; } }

  .cmd-bar {
    display: flex; align-items: center; gap: 8px; margin: 10px 2px 12px;
    padding: 10px 12px; border-radius: 12px;
    background: var(--glass-active, var(--surface)); border: 1px solid var(--glass-border);
    color: var(--ink-faint); font-size: 13px; box-shadow: var(--elev-2);
    transition: transform .14s ease; cursor: pointer;
  }
  .cmd-bar:hover { transform: translateY(-1px); }
  .cmd-bar svg { width: 15px; height: 15px; flex: 0 0 auto; }
  .cmd-bar .kbd { margin-left: auto; font-family: var(--mono); font-size: 11px; color: var(--ink-soft); background: color-mix(in srgb, var(--ink) 6%, transparent); padding: 2px 7px; border-radius: 6px; }
  .sidebar.collapsed .cmd-bar { justify-content: center; padding: 10px 0; }

  .lbl { font-size: 11px; font-weight: 600; letter-spacing: 0.06em; text-transform: uppercase; color: var(--ink-soft); padding: 0 12px 6px; }
  .sidebar.collapsed .lbl { text-align: center; padding: 0 0 6px; }

  .spaces { display: flex; flex-direction: column; gap: 2px; padding: 0 2px 10px; }
  .space { display: flex; align-items: center; gap: 9px; padding: 6px 8px; border-radius: 8px; border: none; background: transparent; text-align: left; color: var(--ink); transition: background .14s ease; }
  .space:hover { background: var(--glass-hover, color-mix(in srgb, var(--ink) 6%, transparent)); }
  .space.sel { background: var(--glass-active, var(--surface)); box-shadow: var(--elev-1); }
  .sidebar.collapsed .space { justify-content: center; padding: 6px; }
  .tile { position: relative; width: 26px; height: 26px; flex: 0 0 26px; }
  .status { position: absolute; right: -1px; bottom: -1px; width: 9px; height: 9px; border-radius: 50%; background: var(--ink-faint); border: 2px solid var(--surface); }
  .status.on { background: var(--online); }
  .space .nm { font-size: 13px; font-weight: 550; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

  .tabs { flex: 1; overflow-y: auto; padding: 0 2px; display: flex; flex-direction: column; gap: 2px; }
  .empty { color: var(--ink-faint); font-size: 12px; padding: 12px; }
  .tab { display: flex; align-items: center; gap: 9px; width: 100%; padding: 7px 8px; border-radius: 8px; border: none; background: transparent; text-align: left; color: var(--ink-soft); transition: background .14s ease; }
  .tab:hover { background: var(--glass-hover, color-mix(in srgb, var(--ink) 6%, transparent)); }
  .tab.sel { background: var(--glass-active, var(--surface)); box-shadow: var(--elev-1); color: var(--ink); }
  .tab .b { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 1px; }
  .r1 { display: flex; align-items: baseline; gap: 6px; }
  .name { font-size: 13px; font-weight: 550; flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .time { font-size: 10px; color: var(--ink-faint); flex: 0 0 auto; font-family: var(--mono); }
  .sub { font-size: 12px; color: var(--ink-faint); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .sub.replying { color: var(--accent-strong, var(--accent)); }

  .foot { padding: 8px 2px 0; margin-top: 6px; border-top: 1px solid var(--hairline-strong, var(--hairline)); position: relative; }
  .fbtn { display: flex; align-items: center; gap: 9px; width: 100%; padding: 8px; border-radius: 8px; border: none; background: transparent; color: var(--ink-soft); font-size: 13px; transition: background .14s ease, color .14s ease; }
  .fbtn svg { width: 17px; height: 17px; flex: 0 0 auto; }
  .fbtn:hover { background: var(--glass-hover, color-mix(in srgb, var(--ink) 6%, transparent)); color: var(--ink); }
  .fbtn:focus-visible { outline: none; box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 30%, transparent); }
  @media (prefers-reduced-motion: reduce) {
    .cmd-bar:hover { transform: none; }
    /* Snap the collapse instead of animating 320ms width/flex transitions —
       theme.css's global reduce-motion neutralizer doesn't override
       per-component transition properties, so this needs to be local. */
    .sidebar { transition: none !important; }
  }
</style>
