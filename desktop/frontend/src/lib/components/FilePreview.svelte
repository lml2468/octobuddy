<script lang="ts">
  import { XClawService } from "../../../bindings/github.com/lml2468/xclaw/desktop";
  import type { FileContent } from "../../../bindings/github.com/lml2468/xclaw/desktop/internal/workspace/models";
  import { renderMarkdown, highlight, onMarkdownCopyClick } from "../markdown";
  import { errMsg } from "../errors";

  let { botId, sessionKey, path, onclose }: {
    botId: string | null;
    sessionKey: string | null;
    path: string;
    onclose: () => void;
  } = $props();

  const isPreview = new URLSearchParams(location.search).has("preview");

  // Preview-mode fixtures so each preview kind can be screenshotted without a daemon.
  const mockFiles: Record<string, FileContent> = {
    "src/main.go": { path: "src/main.go", encoding: "utf8", mime: "text/x-go", kind: "text", truncated: false, size: 220,
      content: `package main\n\nimport "fmt"\n\n// greet returns a greeting for name.\nfunc greet(name string) string {\n\treturn fmt.Sprintf("hello, %s", name)\n}\n\nfunc main() {\n\tfor i := 0; i < 3; i++ {\n\t\tfmt.Println(greet("world"), i)\n\t}\n}\n` } as FileContent,
    "notes.md": { path: "notes.md", encoding: "utf8", mime: "text/markdown", kind: "markdown", truncated: false, size: 180,
      content: "# Notes\n\nThe proto contract is an **NDJSON** envelope over a Unix socket.\n\n- events out\n- commands in\n\n```go\nfunc main() {\n\tapp.Run()\n}\n```\n" } as FileContent,
    "page.html": { path: "page.html", encoding: "utf8", mime: "text/html", kind: "html", truncated: false, size: 240,
      content: "<!doctype html><html><head><style>body{font-family:system-ui;padding:24px;color:#222}h1{color:#07c160}</style></head><body><h1>XClaw HTML preview</h1><p>Rendered in a <strong>sandboxed</strong> iframe.</p><ul><li>one</li><li>two</li></ul></body></html>" } as FileContent,
    "diagram.png": { path: "diagram.png", encoding: "base64", mime: "image/png", kind: "image", truncated: false, size: 95,
      // 1×1 transparent PNG.
      content: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==" } as FileContent,
    "report.pdf": { path: "report.pdf", encoding: "base64", mime: "application/pdf", kind: "pdf", truncated: false, size: 520,
      content: "JVBERi0xLjEKMSAwIG9iajw8L1R5cGUvQ2F0YWxvZy9QYWdlcyAyIDAgUj4+ZW5kb2JqCjIgMCBvYmo8PC9UeXBlL1BhZ2VzL0tpZHNbMyAwIFJdL0NvdW50IDE+PmVuZG9iagozIDAgb2JqPDwvVHlwZS9QYWdlL1BhcmVudCAyIDAgUi9NZWRpYUJveFswIDAgMzAwIDE0NF0vQ29udGVudHMgNCAwIFIvUmVzb3VyY2VzPDwvRm9udDw8L0YxIDUgMCBSPj4+Pj4+ZW5kb2JqCjQgMCBvYmo8PC9MZW5ndGggNjA+PnN0cmVhbQpCVCAvRjEgMTggVGYgMzAgODAgVGQgKFhDbGF3IFBERiBwcmV2aWV3KSBUaiBFVAplbmRzdHJlYW0gZW5kb2JqCjUgMCBvYmo8PC9UeXBlL0ZvbnQvU3VidHlwZS9UeXBlMS9CYXNlRm9udC9IZWx2ZXRpY2E+PmVuZG9iagp0cmFpbGVyPDwvUm9vdCAxIDAgUj4+Cg==" } as FileContent,
  };

  let file = $state<FileContent | null>(null);
  let error = $state("");
  let mdMode = $state<"rendered" | "raw">("rendered");
  let imgFit = $state(true);
  let copied = $state(false);

  // Refetch whenever the target file (or session) changes.
  $effect(() => {
    const b = botId, k = sessionKey, p = path;
    file = null;
    error = "";
    mdMode = "rendered";
    imgFit = true;
    load(b, k, p);
  });

  async function load(b: string | null, k: string | null, p: string) {
    if (isPreview) {
      file = mockFiles[p] ?? ({ path: p, content: "(no preview)", encoding: "utf8", mime: "text/plain", truncated: false, size: 0 } as FileContent);
      return;
    }
    if (!b || !k) return;
    try {
      file = await XClawService.WorkspaceFile(b, k, p);
    } catch (e) {
      error = errMsg(e);
    }
  }

  const name = $derived(path.split("/").pop() ?? path);
  // The backend classifies the file into one kind (markdown/image/pdf/text/binary)
  // — the single source of truth, so we never re-derive from mime/encoding here.
  const kind = $derived(file?.kind ?? "");
  const isMarkdown = $derived(kind === "markdown");
  const isHtml = $derived(kind === "html");
  const isImage = $derived(kind === "image");
  const isPdf = $derived(kind === "pdf");
  const isText = $derived(kind === "text");
  const isBinary = $derived(kind === "binary");

  const mdHtml = $derived(isMarkdown && file ? renderMarkdown(file.content) : "");
  // Code view: line-number gutter + token-highlighted source over one trimmed copy.
  // Also used for the Raw view of markdown/html.
  const trimmed = $derived((isText || isHtml) && file ? file.content.replace(/\n$/, "") : "");
  // Only the line *count* is needed (for the gutter); avoid allocating a full
  // array of line strings for large files.
  const lineCount = $derived(isText && trimmed ? trimmed.split("\n").length : 0);
  const codeHtml = $derived(isText && trimmed ? highlight(trimmed) : "");
  // Raw (source) view for markdown and html shares the highlighter.
  const rawHtml = $derived(
    mdMode === "raw" && file && (isMarkdown || isHtml) ? highlight(file.content) : "",
  );

  // Copy-all is meaningful only for textual content.
  const canCopy = $derived(isText || isMarkdown || isHtml);
  function copyAll() {
    if (!file) return;
    navigator.clipboard?.writeText(file.content);
    copied = true;
    setTimeout(() => (copied = false), 1200);
  }

  // PDFs and rendered HTML render in an <iframe> via a Blob object URL (revoked on
  // change so bytes aren't retained). HTML is agent-written and untrusted, so its
  // iframe is sandboxed (see template) — no scripts, no same-origin access.
  let pdfUrl = $state("");
  $effect(() => {
    if (!(isPdf && file && !file.truncated)) { pdfUrl = ""; return; }
    // file.content from the Go side is base64; a malformed encoding (truncated
    // bytes, non-ASCII chars from a backend bug) makes atob throw, and an
    // unhandled $effect throw tears down the component. Catch + surface as
    // the preview's own error toast instead.
    try {
      const bytes = Uint8Array.from(atob(file.content), (c) => c.charCodeAt(0));
      const url = URL.createObjectURL(new Blob([bytes], { type: "application/pdf" }));
      pdfUrl = url;
      return () => URL.revokeObjectURL(url);
    } catch (e) {
      error = "PDF 解码失败: " + errMsg(e);
      pdfUrl = "";
      return;
    }
  });
  let htmlUrl = $state("");
  $effect(() => {
    if (!(isHtml && file && mdMode === "rendered")) { htmlUrl = ""; return; }
    // text/html;charset=utf-8 so agent-written HTML containing non-ASCII
    // (Chinese filenames, em-dashes, …) renders correctly inside the iframe.
    const url = URL.createObjectURL(new Blob([file.content], { type: "text/html;charset=utf-8" }));
    htmlUrl = url;
    return () => URL.revokeObjectURL(url);
  });

  function fmtSize(n: number): string {
    if (n < 1024) return `${n} B`;
    if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
    return `${(n / (1024 * 1024)).toFixed(1)} MB`;
  }

  function onKey(e: KeyboardEvent) {
    if (e.key === "Escape") onclose();
  }
</script>

<svelte:window onkeydown={onKey} />

<section class="preview">
  <header class="bar">
    <span class="path" title={path}>{name}</span>
    {#if file}<span class="meta">{fmtSize(file.size)}{#if file.truncated} · 已截断{/if}</span>{/if}
    <span class="spacer"></span>

    {#if isMarkdown || isHtml}
      <div class="seg">
        <button class:on={mdMode === "rendered"} onclick={() => (mdMode = "rendered")}>渲染</button>
        <button class:on={mdMode === "raw"} onclick={() => (mdMode = "raw")}>源码</button>
      </div>
    {/if}
    {#if isImage}
      <div class="seg">
        <button class:on={imgFit} onclick={() => (imgFit = true)}>适应</button>
        <button class:on={!imgFit} onclick={() => (imgFit = false)}>原始</button>
      </div>
    {/if}
    {#if canCopy}
      <button class="icon txt" onclick={copyAll}>{copied ? "已复制" : "复制"}</button>
    {/if}
    <button class="icon" title="关闭 (Esc)" aria-label="关闭预览" onclick={onclose}>
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M18 6 6 18M6 6l12 12"/></svg>
    </button>
  </header>

  <div class="body">
    {#if error}
      <div class="msg err" role="alert">加载失败:{error}</div>
    {:else if !file}
      <div class="skel" aria-hidden="true">
        {#each [0, 1, 2, 3, 4, 5] as i (i)}
          <div class="skel-row" style="width:{[64, 88, 72, 80, 56, 76][i]}%"></div>
        {/each}
      </div>
    {:else if isImage}
      <div class="img-wrap"><img class:fit={imgFit} src={`data:${file.mime};base64,${file.content}`} alt={path} /></div>
    {:else if isPdf}
      {#if file.truncated}
        <div class="msg">PDF 太大,无法内联预览({fmtSize(file.size)})。</div>
      {:else if pdfUrl}
        <!-- PDFium is renderer-sandboxed by Chromium, but pin a frame-level
             sandbox so a PDF 0-day can't script the host webview.
             allow-same-origin is needed for the blob: URL viewer chrome. -->
        <iframe class="pdf" title={path} src={pdfUrl} sandbox="allow-same-origin"></iframe>
      {/if}
    {:else if isMarkdown}
      {#if mdMode === "rendered"}
        <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
        <div class="md" onclick={onMarkdownCopyClick}>{@html mdHtml}</div>
      {:else}
        <pre class="code raw"><code>{@html rawHtml}</code></pre>
      {/if}
    {:else if isHtml}
      {#if mdMode === "rendered"}
        <!-- Agent-written HTML is untrusted: sandboxed iframe (no scripts, no
             same-origin) renders it as a page without script execution. -->
        {#if htmlUrl}<iframe class="html" title={path} sandbox="" src={htmlUrl}></iframe>{/if}
      {:else}
        <pre class="code raw"><code>{@html rawHtml}</code></pre>
      {/if}
    {:else if isBinary}
      <div class="msg">二进制文件 — {fmtSize(file.size)},无法预览。</div>
    {:else}
      <!-- code / text: line-number gutter + highlighted source -->
      <div class="code-wrap">
        <pre class="gutter" aria-hidden="true">{#each { length: lineCount } as _, i}{i + 1}{"\n"}{/each}</pre>
        <pre class="code"><code>{@html codeHtml}</code></pre>
      </div>
    {/if}
  </div>
</section>

<style>
  .preview { flex: 1 1 0; min-width: 0; display: flex; flex-direction: column; height: 100%; min-height: 0; background: var(--glass); backdrop-filter: blur(24px) saturate(180%); -webkit-backdrop-filter: blur(24px) saturate(180%); }

  .bar {
    height: var(--header-h); flex: 0 0 var(--header-h);
    display: flex; align-items: center; gap: 10px; padding: 0 12px 0 var(--gutter, 20px);
    background: color-mix(in srgb, var(--surface) 60%, transparent);
    backdrop-filter: blur(20px) saturate(160%); -webkit-backdrop-filter: blur(20px) saturate(160%);
    border-bottom: 1px solid var(--hairline);
  }
  .path { font-family: var(--mono); font-size: 13px; font-weight: 600; color: var(--ink); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; flex: 0 1 auto; }
  .meta { font-family: var(--mono); font-size: 11px; color: var(--ink-faint); flex: 0 0 auto; }
  .spacer { flex: 1; }

  .seg { display: inline-flex; border: 1px solid var(--hairline); border-radius: 6px; overflow: hidden; }
  .seg button {
    font-size: 11.5px; padding: 4px 10px; border: none; background: transparent;
    color: var(--ink-soft); cursor: pointer; transition: background 0.12s ease, color 0.12s ease;
  }
  .seg button:hover { background: color-mix(in srgb, var(--ink) 6%, transparent); }
  .seg button.on { background: color-mix(in srgb, var(--accent) 16%, transparent); color: var(--accent-strong, var(--accent)); }

  .icon {
    display: inline-flex; align-items: center; justify-content: center;
    height: 28px; min-width: 28px; padding: 0 6px; border-radius: 7px; border: none;
    background: transparent; color: var(--ink-soft); cursor: pointer;
    transition: background 0.14s ease, color 0.14s ease;
  }
  .icon.txt { font-family: var(--mono); font-size: 11px; padding: 0 10px; }
  .icon:hover { background: color-mix(in srgb, var(--ink) 7%, transparent); color: var(--accent); }

  .body { flex: 1 1 0; min-height: 0; overflow: hidden; display: flex; }
  .msg { color: var(--ink-soft); font-size: 13px; padding: 22px var(--gutter, 20px); line-height: 1.5; }
  .msg.err { color: var(--danger); }

  /* Loading skeleton — shimmering lines until the file content lands. */
  .skel { flex: 1 1 0; display: flex; flex-direction: column; gap: 14px; padding: 24px var(--gutter, 28px); align-content: flex-start; }
  .skel-row { height: 13px; border-radius: 6px; background: linear-gradient(90deg, color-mix(in srgb, var(--ink) 6%, transparent) 25%, color-mix(in srgb, var(--ink) 11%, transparent) 37%, color-mix(in srgb, var(--ink) 6%, transparent) 63%); background-size: 280% 100%; animation: shimmer 1.4s ease-in-out infinite; }
  @keyframes shimmer { 0% { background-position: 180% 0; } 100% { background-position: -120% 0; } }
  @media (prefers-reduced-motion: reduce) { .skel-row { animation: none; } }

  /* Code / text: shared-scroll gutter + source. */
  .code-wrap { flex: 1 1 0; min-height: 0; display: flex; overflow: auto; background: var(--code-bg); }
  .gutter {
    margin: 0; padding: 14px 10px 14px 14px; text-align: right;
    font-family: var(--mono); font-size: 12px; line-height: 1.6;
    color: var(--ink-faint); background: color-mix(in srgb, var(--ink) 3%, transparent);
    border-right: 1px solid var(--hairline); user-select: none; white-space: pre;
    position: sticky; left: 0;
  }
  .code {
    flex: 1 1 0; margin: 0; padding: 14px 16px; overflow: visible;
    font-family: var(--mono); font-size: 12px; line-height: 1.6; color: var(--ink);
    white-space: pre; tab-size: 2;
  }
  .code.raw { flex: 1 1 0; overflow: auto; background: var(--code-bg); }

  /* token colors (match chat code blocks). */
  .code :global(.tok-kw) { color: var(--tok-kw); }
  .code :global(.tok-str) { color: var(--tok-str); }
  .code :global(.tok-num) { color: var(--tok-num); }
  .code :global(.tok-com) { color: var(--ink-faint); font-style: italic; }

  /* Image. */
  .img-wrap {
    flex: 1 1 0; min-height: 0; overflow: auto; padding: 20px; display: grid; place-items: center;
    background:
      conic-gradient(from 45deg, color-mix(in srgb, var(--ink) 7%, transparent) 25%, transparent 0 50%, color-mix(in srgb, var(--ink) 7%, transparent) 0 75%, transparent 0) 0 0 / 18px 18px;
  }
  .img-wrap img { image-rendering: auto; box-shadow: 0 1px 6px rgba(0,0,0,0.18); }
  .img-wrap img.fit { max-width: 100%; max-height: 100%; height: auto; }

  /* PDF. */
  .pdf { flex: 1 1 0; width: 100%; height: 100%; border: none; background: var(--chat); }
  /* Rendered HTML: white canvas (pages assume a default page background). */
  .html { flex: 1 1 0; width: 100%; height: 100%; border: none; background: var(--surface); }

  /* Rendered markdown — mirror Bubble's .md styles. */
  .md { flex: 1 1 0; min-height: 0; overflow: auto; padding: 22px var(--gutter, 28px); max-width: 820px; width: 100%; margin: 0 auto; color: var(--ink); font-size: 14px; line-height: 1.6; }
  .md :global(h1) { font-size: 1.6em; margin: 0 0 14px; }
  .md :global(h2) { font-size: 1.3em; margin: 18px 0 10px; }
  .md :global(h3) { font-size: 1.1em; margin: 16px 0 8px; }
  .md :global(p) { margin: 0 0 10px; }
  .md :global(code) { font-family: var(--mono); font-size: 0.88em; }
  .md :global(:not(pre) > code) { background: color-mix(in srgb, var(--ink) 8%, transparent); padding: 1px 5px; border-radius: 4px; }
  .md :global(a) { color: var(--accent-strong); }
  .md :global(ul), .md :global(ol) { margin: 0 0 10px; padding-left: 22px; }
  .md :global(blockquote) { margin: 0 0 10px; padding-left: 12px; border-left: 3px solid var(--hairline-strong, var(--hairline)); color: var(--ink-soft); }
  .md :global(table) { border-collapse: collapse; margin: 0 0 12px; font-size: 0.92em; }
  .md :global(th), .md :global(td) { border: 1px solid var(--hairline); padding: 5px 10px; text-align: left; }
  /* Code-block + syntax-token rules are shared via lib/styles/markdown.css. */
</style>
