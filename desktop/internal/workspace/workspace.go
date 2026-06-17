// Package workspace exposes a read-only view of a chat session's sandbox
// workspace (~/.xclaw/<botID>/workspace/<hash>) to the desktop app: a bounded
// file tree and per-file contents. The hash is the same one core/sandbox derives
// from the session's (kind, sessionKey); since the desktop doesn't know whether a
// session is a DM or a group, Tree/File try both kinds and use whichever sandbox
// directory exists on disk.
//
// Everything here is read-only and defensive: bot IDs are slug-validated,
// per-file paths are containment-checked (mirroring internal/skills), symlinks
// are never followed (the daemon symlinks the global skills/workflows catalog
// into each sandbox's .claude/, which must not be traversed or escaped), and the
// walk is bounded in depth, entry count, and file size.
package workspace

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/lml2468/xclaw/core/sandbox"
)

// Bounds keep an arbitrarily large or deep workspace from overwhelming the UI or
// the IPC payload. They are generous for normal use and cheap to raise later.
const (
	maxDepth     = 8
	maxEntries   = 2000
	maxFileBytes = 1 << 20 // 1 MiB
)

// Dir is ~/.xclaw (the install root), matching configstore.Dir().
func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw")
}

var slugRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func validSlug(s string) bool { return s != "" && s != "." && s != ".." && slugRe.MatchString(s) }

// Node is a file or directory in the workspace tree. Path is relative to the
// workspace root, forward-slashed; the root node has Path "". Children is nil for
// files and for directories whose contents are deliberately not expanded (.claude
// and anything past the depth cap).
type Node struct {
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	IsDir    bool    `json:"isDir"`
	Children []*Node `json:"children"`
}

// FileContent is one file's body for inline preview. Content is UTF-8 text when
// Encoding is "utf8", or base64-encoded bytes when "base64" (binary/images).
type FileContent struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Encoding  string `json:"encoding"` // "utf8" | "base64"
	Mime      string `json:"mime"`
	Truncated bool   `json:"truncated"`
	Size      int64  `json:"size"`
}

// resolveRoot returns the session's sandbox directory and whether it exists.
// The desktop doesn't carry the session kind, so we try DM then group and use
// whichever directory is present (the kind prefix makes the two hashes distinct,
// so at most one exists; DM wins a pathological tie). When neither exists yet (no
// turn has run), exists is false and dir is the DM candidate path.
func resolveRoot(botID, sessionKey string) (dir string, exists bool, err error) {
	if !validSlug(botID) {
		return "", false, fmt.Errorf("invalid bot id %q", botID)
	}
	base := filepath.Join(Dir(), botID, "workspace")
	for _, k := range []sandbox.Kind{sandbox.KindDM, sandbox.KindGroup} {
		cand := filepath.Join(base, sandbox.SessionDirName(sandbox.SessionCtx{Kind: k, SessionKey: sessionKey}))
		if fi, e := os.Stat(cand); e == nil && fi.IsDir() {
			return cand, true, nil
		}
	}
	dm := sandbox.SessionDirName(sandbox.SessionCtx{Kind: sandbox.KindDM, SessionKey: sessionKey})
	return filepath.Join(base, dm), false, nil
}

// Tree returns the session's workspace as a bounded file tree. When no sandbox
// exists yet (no turn has run), it returns an empty (non-nil) root so the UI can
// show an "empty workspace" state without an error.
func Tree(botID, sessionKey string) (*Node, error) {
	root, exists, err := resolveRoot(botID, sessionKey)
	if err != nil {
		return nil, err
	}
	out := &Node{Name: filepath.Base(root), Path: "", IsDir: true, Children: []*Node{}}
	if !exists {
		return out, nil
	}
	count := 0
	children, err := readDir(root, "", 1, &count)
	if err != nil {
		return nil, err
	}
	out.Children = children
	return out, nil
}

func readDir(abs, rel string, depth int, count *int) ([]*Node, error) {
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	// Dirs first, then case-insensitive name.
	sort.SliceStable(entries, func(i, j int) bool {
		di, dj := entries[i].IsDir(), entries[j].IsDir()
		if di != dj {
			return di
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})

	var nodes []*Node
	for _, e := range entries {
		if *count >= maxEntries {
			break
		}
		*count++
		name := e.Name()
		childRel := name
		if rel != "" {
			childRel = rel + "/" + name
		}
		// Never follow symlinks: a symlinked dir (e.g. .claude/skills/<bundle>)
		// would escape into the global catalog. Surface it as a leaf.
		isSymlink := e.Type()&os.ModeSymlink != 0
		isDir := e.IsDir() && !isSymlink
		n := &Node{Name: name, Path: childRel, IsDir: isDir}
		if isDir && depth < maxDepth && name != ".claude" {
			kids, err := readDir(filepath.Join(abs, name), childRel, depth+1, count)
			if err == nil {
				n.Children = kids
			}
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// resolveIn validates that rel is a clean relative path inside root and returns
// the absolute path. Mirrors internal/skills.resolveInSkill.
func resolveIn(root, rel string) (string, error) {
	rel = filepath.ToSlash(rel)
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("absolute path not allowed: %q", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path escapes workspace: %q", rel)
		}
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes workspace: %q", rel)
	}
	return full, nil
}

// File reads one workspace file for inline preview. It refuses symlinks and
// directories, caps the read at 1 MiB (setting Truncated), and base64-encodes
// non-text content (images, binaries) so the UI can render it via a data URL.
func File(botID, sessionKey, relPath string) (FileContent, error) {
	var fc FileContent
	root, exists, err := resolveRoot(botID, sessionKey)
	if err != nil {
		return fc, err
	}
	if !exists {
		return fc, fmt.Errorf("no workspace yet for this session")
	}
	full, err := resolveIn(root, relPath)
	if err != nil {
		return fc, err
	}
	// Lexical containment isn't enough: an intermediate symlinked component could
	// still escape the sandbox. Resolve symlinks on both sides and re-check in
	// real-path space (this also normalizes /tmp→/private/tmp on macOS). A broken
	// or missing target makes EvalSymlinks fail, which we treat as "not readable".
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fc, err
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		return fc, err
	}
	if real != realRoot && !strings.HasPrefix(real, realRoot+string(os.PathSeparator)) {
		return fc, fmt.Errorf("path escapes workspace: %q", relPath)
	}
	// Lstat (not Stat) so a symlink final component is refused rather than followed.
	fi, err := os.Lstat(full)
	if err != nil {
		return fc, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fc, fmt.Errorf("refusing to read symlink: %q", relPath)
	}
	if fi.IsDir() {
		return fc, fmt.Errorf("not a file: %q", relPath)
	}

	f, err := os.Open(full)
	if err != nil {
		return fc, err
	}
	defer f.Close()
	buf := make([]byte, maxFileBytes+1)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		// Read returns io.EOF (n==0) for an empty file — that's fine, not an error.
		if err.Error() != "EOF" {
			return fc, err
		}
	}
	data := buf[:n]
	truncated := n > maxFileBytes
	if truncated {
		data = data[:maxFileBytes]
	}

	fc = FileContent{
		Path:      filepath.ToSlash(relPath),
		Mime:      mimeOf(full, data),
		Truncated: truncated,
		Size:      fi.Size(),
	}
	if isTextual(fc.Mime, data) {
		fc.Encoding = "utf8"
		fc.Content = string(data)
	} else {
		fc.Encoding = "base64"
		fc.Content = base64.StdEncoding.EncodeToString(data)
	}
	return fc, nil
}

// extMime covers the common code/text/image types by extension; anything else
// falls back to content sniffing.
var extMime = map[string]string{
	".md": "text/markdown", ".txt": "text/plain", ".log": "text/plain",
	".go": "text/x-go", ".py": "text/x-python", ".rs": "text/x-rust",
	".js": "application/javascript", ".mjs": "application/javascript",
	".ts": "text/x-typescript", ".tsx": "text/x-typescript", ".jsx": "text/javascript",
	".json": "application/json", ".yaml": "application/yaml", ".yml": "application/yaml",
	".toml": "text/x-toml", ".ini": "text/plain", ".env": "text/plain",
	".html": "text/html", ".css": "text/css", ".svg": "image/svg+xml",
	".sh": "text/x-shellscript", ".sql": "text/x-sql", ".csv": "text/csv",
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".bmp": "image/bmp", ".ico": "image/x-icon",
}

func mimeOf(path string, data []byte) string {
	if m, ok := extMime[strings.ToLower(filepath.Ext(path))]; ok {
		return m
	}
	return http.DetectContentType(data)
}

// isTextual decides whether to send the file as UTF-8 text (rendered in <pre>) or
// base64 (rendered as an image / download). SVG is text but treated as an image
// by the UI via its image/* mime.
func isTextual(mime string, data []byte) bool {
	if strings.HasPrefix(mime, "image/") {
		return false
	}
	if strings.HasPrefix(mime, "text/") ||
		strings.HasSuffix(mime, "+json") || strings.HasSuffix(mime, "+xml") ||
		mime == "application/json" || mime == "application/javascript" || mime == "application/yaml" {
		return true
	}
	// Fallback: valid UTF-8 with no NUL byte is treated as text.
	if !utf8.Valid(data) {
		return false
	}
	for _, b := range data {
		if b == 0 {
			return false
		}
	}
	return true
}
