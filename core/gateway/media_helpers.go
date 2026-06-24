package gateway

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/lml2468/octobuddy/core/safepath"
)

// RenderImageFragment returns the prompt line listing downloaded image paths
// in the format the agent already knows from IM inbound — a single
// "[已下载图片到本地: …]" hint for one image, or a "[已下载 N 张图片到本地: …]"
// hint for multiple. Empty for an empty slice. Composer attachments and IM
// inbound both route through here so the agent sees one consistent shape.
func RenderImageFragment(relPaths []string) string {
	switch len(relPaths) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("[已下载图片到本地: %s — 请用 Read 工具查看]", relPaths[0])
	default:
		return fmt.Sprintf("[已下载 %d 张图片到本地: %s — 请用 Read 工具逐个查看]",
			len(relPaths), strings.Join(relPaths, ", "))
	}
}

// RenderFileFragment returns the prompt block for a downloaded file —
// "[文件: name]\n本地路径: relPath". Mirrors resolveFile's downloaded-file
// branch; shared so Composer attachments produce an identical line.
func RenderFileFragment(name, relPath string) string {
	return fmt.Sprintf("[文件: %s]\n本地路径: %s", name, relPath)
}

// RenderInlinedFileFragment wraps a small text-like file's bytes in a
// `<file_content name=… encoding="base64" bytes="…">…</file_content>` block —
// the same envelope IM-inbound uses for text files under inlineFileMaxBytes.
// Caller must check ShouldInlineAsText + the size cap.
func RenderInlinedFileFragment(name string, body []byte) string {
	return buildInlinedFileBody(name, body)
}

// ShouldInlineAsText reports whether a filename's extension is in the
// text-like set the inbound path inlines (vs writes-then-references). Shared
// so Composer's branching mirrors inbound's.
func ShouldInlineAsText(name string) bool {
	return textFileExtensions[extractExtension("", name)]
}

// MaxInlineFileBytes is the size cap (in bytes) above which a text-like
// attachment is materialized to disk instead of inlined in the prompt.
// Exposed so Composer-path callers can branch identically to inbound.
const MaxInlineFileBytes = inlineFileMaxBytes

// MaxImageBytes is the per-attachment image size cap shared by IM inbound and
// Composer outbound. Bytes over this cap are rejected at write time.
const MaxImageBytes = maxImageBytes

// MaxFileBytes is the per-attachment file (non-image) size cap shared by IM
// inbound and Composer outbound.
const MaxFileBytes = maxFileDownloadBytes

// MaxImagesPerSend is the per-message image cap (IM inbound enforces it as
// MAX_IMAGES_PER_MESSAGE; Composer respects the same).
const MaxImagesPerSend = maxImagesPerMessage

// WriteSandboxImage SafeWrites the given image bytes into
// <cwd>/.octobuddy-media/<uuid>-image.<ext> and returns the cwd-relative path
// the prompt fragment uses. The Composer path calls this with bytes the
// operator picked locally (no HTTP fetch). Mime must be in allowedImageTypes;
// body must be <= MaxImageBytes and non-empty.
func WriteSandboxImage(cwd, mime string, body []byte) (string, error) {
	rawType := strings.ToLower(strings.TrimSpace(mime))
	ext, ok := allowedImageTypes[rawType]
	if !ok {
		if rawType == "" {
			rawType = "未知"
		}
		return "", fmt.Errorf("unsupported image type: %s", rawType)
	}
	if err := checkAttachmentSize(body, MaxImageBytes); err != nil {
		return "", err
	}
	if err := safepath.SafeMkdirAll(cwd, InboundMediaDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir media dir: %w", err)
	}
	name := fmt.Sprintf("%s-image.%s", uuid.NewString(), ext)
	if err := safepath.SafeWrite(cwd, filepath.Join(InboundMediaDir, name), body, 0o600); err != nil {
		return "", fmt.Errorf("write image: %w", err)
	}
	return filepath.Join(InboundMediaDir, name), nil
}

// WriteSandboxFile SafeWrites the given file bytes into
// <cwd>/.octobuddy-media/<uuid>-<safename> and returns the cwd-relative path
// the prompt fragment uses. The operator-supplied name is sanitized to
// [A-Za-z0-9._-] for the on-disk leaf — the original name is preserved for
// the prompt label by callers.
func WriteSandboxFile(cwd, name string, body []byte) (string, error) {
	if err := checkAttachmentSize(body, MaxFileBytes); err != nil {
		return "", err
	}
	if err := safepath.SafeMkdirAll(cwd, InboundMediaDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir media dir: %w", err)
	}
	safeName := sanitizeFileBaseName(name)
	leaf := fmt.Sprintf("%s-%s", uuid.NewString(), safeName)
	if err := safepath.SafeWrite(cwd, filepath.Join(InboundMediaDir, leaf), body, 0o600); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}
	return filepath.Join(InboundMediaDir, leaf), nil
}

func checkAttachmentSize(body []byte, max int64) error {
	if len(body) == 0 {
		return fmt.Errorf("empty attachment")
	}
	if int64(len(body)) > max {
		return fmt.Errorf("exceeds size cap %s", formatBytes(max))
	}
	return nil
}

// writeCapped reads src into memory (capped at max bytes) and writes it via
// safepath.SafeWrite anchored at `root` — symlink-safe (refuses a planted
// leaf-symlink AND dirfd-walks every component from `root`), atomic
// temp+rename. Caller passes `root` = the operator-trusted sandbox cwd and
// `rel` = the leaf path inside it; for cwdBase configured outside $HOME
// SafeWriteAbs's fallback skipped the dirfd walk entirely, leaving the
// .octobuddy-media exfil race open. Taking root explicitly closes that path.
func writeCapped(root, rel string, src io.Reader, max int64) error {
	buf, err := io.ReadAll(io.LimitReader(src, max+1))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if int64(len(buf)) > max {
		return fmt.Errorf("exceeds size cap %s", formatBytes(max))
	}
	if len(buf) == 0 {
		return fmt.Errorf("empty response")
	}
	if err := safepath.SafeWrite(root, rel, buf, 0o600); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	return nil
}

// buildInlinedFileBody frames file content as a base64 <file_content> block.
// The base64 alphabet ([A-Za-z0-9+/=]) contains none of <, /, > so the content
// cannot forge the closing tag (file-inline-wrap.ts S2 defense).
func buildInlinedFileBody(filename string, content []byte) string {
	safeName := sanitizeFilenameForAttribute(filename)
	b64 := base64.StdEncoding.EncodeToString(content)
	return fmt.Sprintf("[文件: %s]\n<file_content name=%q encoding=\"base64\" bytes=\"%d\">\n%s\n</file_content>",
		filename, safeName, len(content), b64)
}

// sanitizeFilenameForAttribute strips characters that could break out of the
// name="..." attribute or be misread as the closing tag (file-inline-wrap.ts).
func sanitizeFilenameForAttribute(name string) string {
	r := strings.NewReplacer(
		"<", "_", ">", "_", "\"", "_", "'", "_", "\\", "_",
		"\r", "_", "\n", "_", "\t", "_",
	)
	out := r.Replace(name)
	if len(out) > 128 {
		out = out[:128]
	}
	return out
}

// sanitizeFileBaseName reduces a filename to [A-Za-z0-9._-] for an on-disk name
// (tryResolveFile temp-path naming), falling back to "file".
func sanitizeFileBaseName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "file"
	}
	s := b.String()
	// A name that is only dots (".", "..", …) is harmless here because callers
	// always prefix a uuid, but collapse it anyway so an on-disk name never reads
	// like a path traversal segment.
	if strings.Trim(s, ".") == "" {
		return "file"
	}
	return s
}

// extractExtension returns the lowercase extension from the URL path, falling
// back to the filename. Matches extractExtension in inbound.ts (<=8 chars).
func extractExtension(rawURL, fallbackName string) string {
	if u, err := url.Parse(rawURL); err == nil {
		if ext := extFromPath(u.Path); ext != "" {
			return ext
		}
	}
	return extFromPath(fallbackName)
}

func extFromPath(p string) string {
	// Drop any query/fragment that rode along on a fallback name (the URL-parsed
	// path is already clean, but a raw filename may carry "?v=2" etc.).
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	i := strings.LastIndex(p, ".")
	if i < 0 || i == len(p)-1 {
		return ""
	}
	ext := strings.ToLower(p[i+1:])
	// stop at the first path-ish separator that slipped in
	if strings.ContainsAny(ext, "/\\") || len(ext) > 8 {
		return ""
	}
	return ext
}

// formatBytes renders a byte count for a human-readable label (formatBytes in
// inbound.ts).
func formatBytes(n int64) string {
	const (
		kb = 1024
		mb = 1024 * 1024
		gb = 1024 * 1024 * 1024
	)
	switch {
	case n < kb:
		return fmt.Sprintf("%dB", n)
	case n < mb:
		return fmt.Sprintf("%.1fKB", float64(n)/kb)
	case n < gb:
		return fmt.Sprintf("%.1fMB", float64(n)/mb)
	default:
		return fmt.Sprintf("%.1fGB", float64(n)/gb)
	}
}
