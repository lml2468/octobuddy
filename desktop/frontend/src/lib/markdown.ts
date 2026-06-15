import { marked } from "marked";
import DOMPurify from "dompurify";

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
const KW = "func|return|if|else|for|while|switch|case|break|continue|let|var|const|def|class|struct|enum|interface|type|import|export|from|package|public|private|protected|static|new|try|catch|finally|throw|async|await|yield|nil|null|undefined|true|false|self|this|void|fn|use|pub|mut|match|do|end|module|defer|go|chan|map|range|select";
const TOKEN = new RegExp(
  "(\\/\\/[^\\n]*|#[^\\n]*|\\/\\*[\\s\\S]*?\\*\\/)" + // comments
  "|(\"(?:[^\"\\\\\\n]|\\\\.)*\"|'(?:[^'\\\\\\n]|\\\\.)*'|`(?:[^`\\\\]|\\\\.)*`)" + // strings
  "|\\b(" + KW + ")\\b" + // keywords
  "|\\b(0x[0-9a-fA-F]+|\\d[\\d_.]*)\\b", // numbers
  "g",
);

function highlight(code: string): string {
  let out = "", last = 0, m: RegExpExecArray | null;
  TOKEN.lastIndex = 0;
  while ((m = TOKEN.exec(code))) {
    out += esc(code.slice(last, m.index));
    const cls = m[1] ? "tok-com" : m[2] ? "tok-str" : m[3] ? "tok-kw" : "tok-num";
    out += `<span class="${cls}">${esc(m[0])}</span>`;
    last = m.index + m[0].length;
  }
  out += esc(code.slice(last));
  return out;
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
