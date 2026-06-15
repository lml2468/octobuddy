import { marked } from "marked";
import DOMPurify from "dompurify";

// Render agent Markdown to sanitized HTML, memoized by source text so scrolling
// and re-renders are O(1) (mirrors the Swift MarkdownRenderer NSCache). The agent
// is semi-trusted, but we sanitize anyway — arbitrary HTML in a webview is XSS.
const cache = new Map<string, string>();
const MAX = 400;

marked.setOptions({ gfm: true, breaks: true });

export function renderMarkdown(src: string): string {
  const hit = cache.get(src);
  if (hit !== undefined) return hit;
  const raw = marked.parse(src, { async: false }) as string;
  const clean = DOMPurify.sanitize(raw, {
    ADD_ATTR: ["target"],
  });
  if (cache.size >= MAX) {
    // Evict oldest insertion (Map preserves insertion order).
    const first = cache.keys().next().value;
    if (first !== undefined) cache.delete(first);
  }
  cache.set(src, clean);
  return clean;
}
