import { marked } from "marked";
import DOMPurify from "dompurify";

// Harden any anchor DOMPurify keeps: force rel="noopener noreferrer" on links
// that carry a target (defends against reverse-tabnabbing if a target ever
// appears), and drop href entirely for non-http(s)/mailto schemes so a
// javascript:/data: URL in agent output can't become a clickable vector. Runs
// once at module load; applies to every sanitize call below.
DOMPurify.addHook("afterSanitizeAttributes", (node) => {
  if (node.tagName === "A") {
    const el = node as Element;
    const href = (el.getAttribute("href") ?? "").trim();
    // Allow only safe schemes / fragments / SAME-PAGE absolute paths.
    // A bare leading slash that starts with `//` is the protocol-relative
    // form (`//evil.com`) which in a remote-rendered context becomes
    // attacker-origin XHR/navigation — reject. The webview today resolves
    // it harmlessly to `wails://…` but the rule is defense-in-depth for
    // any future rendering surface.
    const safe =
      /^https?:/i.test(href) ||
      /^mailto:/i.test(href) ||
      href.startsWith("#") ||
      (href.startsWith("/") && !href.startsWith("//"));
    if (href && !safe) {
      el.removeAttribute("href");
    }
    if (el.getAttribute("target")) {
      el.setAttribute("rel", "noopener noreferrer");
    }
  }
});

// Render agent Markdown to sanitized HTML, memoized per source text so scrolling
// is O(1). Fenced code gets a header (language + copy) and lightweight,
// language-agnostic syntax tinting — the "developer instrument" detail.
const cache = new Map<string, string>();
const MAX = 400;

function esc(s: string): string {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

// Tokenize raw code and wrap comments / strings / keywords / numbers. Operates on
// the raw text (escaping each chunk) so HTML entities never break the regex.
// `highlight` is exported so non-markdown surfaces (the workspace file preview)
// get the same language-agnostic tinting as chat code blocks.
const KW = "func|return|if|else|for|while|switch|case|break|continue|let|var|const|def|class|struct|enum|interface|type|import|export|from|package|public|private|protected|static|new|try|catch|finally|throw|async|await|yield|nil|null|undefined|true|false|self|this|void|fn|use|pub|mut|match|do|end|module|defer|go|chan|map|range|select";
const TOKEN = new RegExp(
  "(\\/\\/[^\\n]*|#[^\\n]*|\\/\\*[\\s\\S]*?\\*\\/)" + // comments
  "|(\"(?:[^\"\\\\\\n]|\\\\.)*\"|'(?:[^'\\\\\\n]|\\\\.)*'|`(?:[^`\\\\]|\\\\.)*`)" + // strings
  "|\\b(" + KW + ")\\b" + // keywords
  "|\\b(0x[0-9a-fA-F]+|\\d[\\d_.]*)\\b", // numbers
  "g",
);

export function highlight(code: string): string {
  const hit = hlCache.get(code);
  if (hit !== undefined) return hit;
  let out = "", last = 0, m: RegExpExecArray | null;
  TOKEN.lastIndex = 0;
  while ((m = TOKEN.exec(code))) {
    out += esc(code.slice(last, m.index));
    const cls = m[1] ? "tok-com" : m[2] ? "tok-str" : m[3] ? "tok-kw" : "tok-num";
    out += `<span class="${cls}">${esc(m[0])}</span>`;
    last = m.index + m[0].length;
  }
  out += esc(code.slice(last));
  if (hlCache.size >= MAX) {
    const first = hlCache.keys().next().value;
    if (first !== undefined) hlCache.delete(first);
  }
  hlCache.set(code, out);
  return out;
}
const hlCache = new Map<string, string>();

// Delegated copy handler for code blocks rendered via {@html} (the `.cb-copy`
// button). Shared by every surface that renders markdown (chat bubbles, the
// workspace file preview) so the copy affordance behaves identically.
export function onMarkdownCopyClick(e: MouseEvent): void {
  const btn = (e.target as HTMLElement).closest(".cb-copy");
  if (!btn) return;
  const code = btn.closest(".codeblock")?.querySelector("code");
  if (!code) return;
  navigator.clipboard?.writeText(code.textContent ?? "");
  btn.textContent = "copied";
  setTimeout(() => (btn.textContent = "copy"), 1200);
}

marked.setOptions({ gfm: true, breaks: true });
marked.use({
  renderer: {
    code(codeOrTok: any, infostring?: string) {
      const text = typeof codeOrTok === "object" ? codeOrTok.text : codeOrTok;
      const lang = ((typeof codeOrTok === "object" ? codeOrTok.lang : infostring) || "").split(/\s+/)[0];
      return `<div class="codeblock"><div class="cb-head"><span class="cb-lang">${esc(lang || "code")}</span><button class="cb-copy" type="button" aria-label="Copy code">copy</button></div><pre><code>${highlight(text)}</code></pre></div>`;
    },
  },
});

export function renderMarkdown(src: string): string {
  const hit = cache.get(src);
  if (hit !== undefined) return hit;
  const raw = marked.parse(src, { async: false }) as string;
  const clean = DOMPurify.sanitize(raw, { ADD_ATTR: ["target"] });
  if (cache.size >= MAX) {
    const first = cache.keys().next().value;
    if (first !== undefined) cache.delete(first);
  }
  cache.set(src, clean);
  return clean;
}
