<script lang="ts">
  import { XClawService } from "../../../bindings/github.com/lml2468/xclaw/desktop";
  import type { Node } from "../../../bindings/github.com/lml2468/xclaw/desktop/internal/workspace/models";

  let { botId, sessionKey, activePath, onopen, onclose }: {
    botId: string | null;
    sessionKey: string | null;
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
    const b = botId, k = sessionKey;
    expanded = new Set();
    loadTree(b, k);
  });

  async function loadTree(b: string | null, k: string | null) {
    error = "";
    if (!b || !k) { tree = null; return; }
    loading = true;
    try {
      tree = isPreview ? mockTree : await XClawService.WorkspaceTree(b, k);
    } catch (e: any) {
      error = String(e?.message ?? e);
      tree = null;
    } finally {
      loading = false;
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
</script>

<div class="panel">
  <header class="bar">
    <span class="title">Workspace</span>
    <span class="spacer"></span>
    <button class="icon" title="Refresh" aria-label="Refresh" onclick={() => loadTree(botId, sessionKey)}>
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12a9 9 0 1 0 3-6.7L3 8"/><path d="M3 3v5h5"/></svg>
    </button>
    <button class="icon" title="Close" aria-label="Close workspace" onclick={onclose}>
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M18 6 6 18M6 6l12 12"/></svg>
    </button>
  </header>

  <div class="tree">
    {#if error}
      <div class="msg err">{error}</div>
    {:else if loading && !tree}
      <div class="msg">Loading…</div>
    {:else if !hasFiles}
      <div class="msg">No files yet. The agent's workspace appears here once it writes something.</div>
    {:else}
      {#each kids(tree) as child (child.path)}
        {@render row(child, 0)}
      {/each}
    {/if}
  </div>
</div>

{#snippet row(node: Node, depth: number)}
  {#if node.isDir}
    <button class="node dir" style="padding-left:{8 + depth * 14}px" onclick={() => toggle(node.path)}>
      <span class="chev" class:open={expanded.has(node.path)} class:hidden={node.children == null}>▸</span>
      <span class="ico">📁</span>
      <span class="name">{node.name}</span>
    </button>
    {#if expanded.has(node.path) && node.children}
      {#each kids(node) as c (c.path)}
        {@render row(c, depth + 1)}
      {/each}
    {/if}
  {:else}
    <button class="node file" class:sel={node.path === activePath} style="padding-left:{8 + depth * 14 + 14}px" onclick={() => onopen(node.path)}>
      <span class="name">{node.name}</span>
    </button>
  {/if}
{/snippet}

<style>
  .panel { display: flex; flex-direction: column; height: 100%; min-height: 0; }
  .bar {
    height: var(--header-h); flex: 0 0 var(--header-h);
    display: flex; align-items: center; gap: 6px; padding: 0 12px 0 16px;
    background: var(--surface); border-bottom: 1px solid var(--hairline);
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

  .tree { flex: 1 1 0; min-height: 0; overflow: auto; padding: 6px 0; }
  .msg { color: var(--ink-soft); font-size: 12px; padding: 14px 16px; line-height: 1.5; }
  .msg.err { color: var(--danger); }

  .node {
    display: flex; align-items: center; gap: 6px; width: 100%;
    padding: 4px 10px 4px 8px; border: none; background: transparent;
    text-align: left; cursor: pointer; color: var(--ink);
    font-size: 12.5px; line-height: 1.4;
    transition: background 0.1s ease;
  }
  .node:hover { background: color-mix(in srgb, var(--ink) 6%, transparent); }
  .node.file.sel { background: color-mix(in srgb, var(--accent) 16%, transparent); color: var(--accent-strong, var(--accent)); }
  .chev { width: 10px; color: var(--ink-faint); transition: transform 0.12s ease; flex: 0 0 10px; font-size: 10px; }
  .chev.open { transform: rotate(90deg); }
  .chev.hidden { visibility: hidden; }
  .ico { flex: 0 0 auto; font-size: 11px; opacity: 0.85; }
  .name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
