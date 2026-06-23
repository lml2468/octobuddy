<script lang="ts">
  import { XClawService } from "../../../bindings/github.com/lml2468/xclaw/desktop";
  import type { Node } from "../../../bindings/github.com/lml2468/xclaw/desktop/internal/workspace/models";
  import { errMsg } from "../errors";

  type FileSource = "workspace" | "memory";

  let { botId, channelType = 1, sessionKey, source = "workspace", activePath, onopen, onclose }: {
    botId: string | null;
    channelType?: number;
    sessionKey: string | null;
    source?: FileSource;
    activePath: string | null;
    onopen: (path: string) => void;
    onclose: () => void;
  } = $props();

  const isPreview = new URLSearchParams(location.search).has("preview");

  let tree = $state<Node | null>(null);
  let expanded = $state<Set<string>>(new Set());
  let error = $state("");
  let loading = $state(false);

 // Preview-mode mock so the layout can be screenshotted without a daemon.
  const mockTree = {
    name: "workspace", path: "", isDir: true,
    children: [
      { name: "src", path: "src", isDir: true, children: [
        { name: "main.go", path: "src/main.go", isDir: false, children: null },
      ] },
      { name: "notes.md", path: "notes.md", isDir: false, children: null },
      { name: "page.html", path: "page.html", isDir: false, children: null },
      { name: "diagram.png", path: "diagram.png", isDir: false, children: null },
      { name: "report.pdf", path: "report.pdf", isDir: false, children: null },
    ],
  } as unknown as Node;

 // Refetch whenever the selected session changes (covers open + switch).
  $effect(() => {
    const b = botId, ct = channelType, k = sessionKey, src = source;
    expanded = new Set();
    loadTree(b, ct, k, src);
  });

 // Generation counter discards stale fetches: switching sessions twice
 // quickly used to leave the slower (older) WorkspaceTree response
 // overwriting `tree` with the wrong session's files.
  let loadGen = 0;
  async function loadTree(b: string | null, ct: number, k: string | null, src: FileSource) {
    const gen = ++loadGen;
    error = "";
    if (!b || !k) {
 // Switching back to a selection-less state mid-fetch: clear the
 // tree AND drop the spinner. Previously the early-return left
 // `loading=true` from a still-running prior fetch — its finally
 // skipped the reset (gen mismatch) and the spinner spun forever
 // until the next non-null load.
      tree = null;
      loading = false;
      return;
    }
    loading = true;
    try {
      const t = isPreview
        ? mockTree
        : src === "memory"
          ? await XClawService.MemoryTree(b, ct, k)
          : await XClawService.WorkspaceTree(b, ct, k);
      if (gen === loadGen) tree = t;
    } catch (e) {
      if (gen === loadGen) { error = errMsg(e); tree = null; }
    } finally {
      if (gen === loadGen) loading = false;
    }
  }

  function toggle(path: string) {
    const next = new Set(expanded);
    next.has(path) ? next.delete(path) : next.add(path);
    expanded = next; // Svelte 5: reassign, don't mutate in place
  }

 // Generated children type is (Node | null)[]; narrow to non-null Node[].
  function kids(n: Node | null): Node[] {
    return ((n?.children ?? []) as (Node | null)[]).filter((c): c is Node => c != null);
  }

  const hasFiles = $derived(kids(tree).length > 0);
  const title = $derived(source === "memory" ? "记忆" : "工作区");
  const emptyText = $derived(source === "memory" ? "还没有记忆。Agent 写入 session memory 后会显示在这里。" : "还没有文件。Agent 写入工作区后会显示在这里。");
</script>

<div class="panel">
  <header class="bar">
    <span class="title">{title}</span>
    <span class="spacer"></span>
    <button class="icon" class:spin={loading} title="刷新" aria-label="刷新" onclick={() => loadTree(botId, channelType, sessionKey, source)}>
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12a9 9 0 1 0 3-6.7L3 8"/><path d="M3 3v5h5"/></svg>
    </button>
    <button class="icon" title="关闭" aria-label={`关闭${title}`} onclick={onclose}>
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M18 6 6 18M6 6l12 12"/></svg>
    </button>
  </header>

  <div class="tree">
    {#if error}
      <div class="msg err" role="alert">
        <span>加载失败:{error}</span>
        <button class="retry" onclick={() => loadTree(botId, channelType, sessionKey, source)}>重试</button>
      </div>
    {:else if loading && !tree}
      <div class="skel" aria-hidden="true">
        {#each [0, 1, 2, 3, 4] as i (i)}
          <div class="skel-row" style="width:{[78, 60, 70, 52, 66][i]}%"></div>
        {/each}
      </div>
    {:else if !hasFiles}
      <div class="msg">{emptyText}</div>
    {:else}
      {#each kids(tree) as child (child.path)}
        {@render row(child, 0)}
      {/each}
    {/if}
  </div>
</div>

{#snippet row(node: Node, depth: number)}
  {#if node.isDir}
    <button class="node dir" style="padding-left:{8 + depth * 14}px" onclick={() => toggle(node.path)} aria-expanded={expanded.has(node.path)}>
      <span class="chev" class:open={expanded.has(node.path)} class:hidden={node.children == null}>
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="m9 18 6-6-6-6"/></svg>
      </span>
      <span class="ico"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg></span>
      <span class="name">{node.name}</span>
    </button>
    {#if expanded.has(node.path) && node.children}
      {#each kids(node) as c (c.path)}
        {@render row(c, depth + 1)}
      {/each}
      {#if node.children.length === 0}
        <div class="empty-leaf" style="padding-left:{8 + (depth + 1) * 14}px">空目录</div>
      {/if}
    {/if}
  {:else}
    <button class="node file" class:sel={node.path === activePath} style="padding-left:{8 + depth * 14 + 14}px" onclick={() => onopen(node.path)}>
      <span class="ico"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><path d="M14 3v4a1 1 0 0 0 1 1h4"/><path d="M5 3h9l5 5v11a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2z"/></svg></span>
      <span class="name">{node.name}</span>
    </button>
  {/if}
{/snippet}

<style>
  .panel { display: flex; flex-direction: column; height: 100%; min-height: 0; }
  .bar {
    height: var(--header-h); flex: 0 0 var(--header-h);
    display: flex; align-items: center; gap: 6px; padding: 0 12px 0 16px;
    background: color-mix(in srgb, var(--surface) 60%, transparent);
    backdrop-filter: blur(20px) saturate(160%); -webkit-backdrop-filter: blur(20px) saturate(160%);
    border-bottom: 1px solid var(--hairline);
  }
  .title { font-size: 13px; font-weight: 600; color: var(--ink); }
  .spacer { flex: 1; }
  .icon {
    display: inline-flex; align-items: center; justify-content: center;
    width: 28px; height: 28px; border-radius: 7px; border: none;
    background: transparent; color: var(--ink-soft); cursor: pointer;
    transition: background 0.14s ease, color 0.14s ease;
  }
  .icon:hover { background: color-mix(in srgb, var(--ink) 7%, transparent); color: var(--accent); }
  .icon.spin svg { animation: spin 0.9s linear infinite; }
  @keyframes spin { to { transform: rotate(360deg); } }

  .tree { flex: 1 1 0; min-height: 0; overflow: auto; padding: 6px 0; }
  .msg { color: var(--ink-soft); font-size: 12px; padding: 14px 16px; line-height: 1.5; }
  .msg.err { color: var(--danger); display: flex; flex-direction: column; align-items: flex-start; gap: 8px; }
  .retry { font-size: 12px; padding: 5px 12px; border-radius: 8px; border: 1px solid color-mix(in srgb, var(--danger) 40%, var(--hairline)); background: transparent; color: var(--danger); cursor: pointer; transition: background 0.14s ease; }
  .retry:hover { background: color-mix(in srgb, var(--danger) 10%, transparent); }
  .retry:focus-visible { outline: none; box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 30%, transparent); }

 /* Loading skeleton — shimmering placeholder rows until the tree lands. */
  .skel { display: flex; flex-direction: column; gap: 12px; padding: 16px; }
  .skel-row { height: 12px; border-radius: 6px; background: linear-gradient(90deg, color-mix(in srgb, var(--ink) 6%, transparent) 25%, color-mix(in srgb, var(--ink) 11%, transparent) 37%, color-mix(in srgb, var(--ink) 6%, transparent) 63%); background-size: 280% 100%; animation: shimmer 1.4s ease-in-out infinite; }
  @keyframes shimmer { 0% { background-position: 180% 0; } 100% { background-position: -120% 0; } }

  .node {
    display: flex; align-items: center; gap: 6px; width: 100%;
    padding: 4px 10px 4px 8px; border: none; background: transparent;
    text-align: left; cursor: pointer; color: var(--ink);
    font-size: 12.5px; line-height: 1.4;
    transition: background 0.1s ease;
  }
  .node:hover { background: color-mix(in srgb, var(--ink) 6%, transparent); }
  .node:focus-visible { outline: none; box-shadow: inset 0 0 0 2px color-mix(in srgb, var(--accent) 35%, transparent); }
  .node.file.sel { background: color-mix(in srgb, var(--accent) 16%, transparent); color: var(--accent-strong, var(--accent)); }
  .chev { width: 12px; height: 12px; color: var(--ink-faint); transition: transform 0.12s ease; flex: 0 0 12px; display: grid; place-items: center; }
  .chev svg { width: 12px; height: 12px; }
  .chev.open { transform: rotate(90deg); }
  .chev.hidden { visibility: hidden; }
  .ico { flex: 0 0 auto; width: 15px; height: 15px; color: var(--ink-faint); display: grid; place-items: center; }
  .ico svg { width: 15px; height: 15px; }
  .node.file.sel .ico { color: var(--accent-strong, var(--accent)); }
  .name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .empty-leaf { font-size: 12px; color: var(--ink-faint); padding: 4px 8px; font-style: italic; }
  @media (prefers-reduced-motion: reduce) {
    .skel-row { animation: none; }
    .icon.spin svg { animation-duration: 1.6s; }
  }
</style>
