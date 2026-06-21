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
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/lml2468/xclaw/core/sandbox"
	"github.com/lml2468/xclaw/desktop/internal/safepath"
)

// Bounds keep an arbitrarily large or deep workspace from overwhelming the UI or
// the IPC payload. They are generous for normal use and cheap to raise later.
const (
	maxDepth   = 8
	maxEntries = 2000
	// maxTextBytes caps text files sent as UTF-8 for inline preview. maxBinaryBytes
	// is the larger cap for base64 content (images, PDFs) — those need the whole
	// file for a valid data-URL, and routinely exceed 1 MiB. We read up to the
	// larger cap, then truncate against the per-kind cap once the kind is known.
	maxTextBytes   = 1 << 20 // 1 MiB
	maxBinaryBytes = 8 << 20 // 8 MiB
)

// Dir is ~/.xclaw (the install root), matching configstore.Dir().
func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw")
}

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
// Kind is the single source of truth for how the UI should render it, derived
// here from the mime + encoding so the frontend never re-classifies.
type FileContent struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Encoding  string `json:"encoding"` // "utf8" | "base64"
	Mime      string `json:"mime"`
	Kind      string `json:"kind"` // "markdown" | "image" | "pdf" | "text" | "binary"
	Truncated bool   `json:"truncated"`
	Size      int64  `json:"size"`
}

// resolveRoot returns the session's sandbox directory and whether it exists.
// The desktop doesn't carry the session kind, so we try DM then group and use
// whichever directory is present (the kind prefix makes the two hashes distinct,
// so at most one exists; DM wins a pathological tie). When neither exists yet (no
// turn has run), exists is false and dir is the DM candidate path.
func resolveRoot(botID, sessionKey string) (dir string, exists bool, err error) {
	if !safepath.ValidSlug(botID) {
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

	// Initialize as an empty (non-nil) slice so JSON marshals an EMPTY but
	// readable directory as `[]` instead of `null`. The frontend uses null to
	// mean "not expandable" (depth-cap / .claude / symlink leaf); without
	// this an empty dir was indistinguishable from a depth-capped one and
	// the chevron silently disappeared.
	nodes := []*Node{}
	for _, e := range entries {
		if *count >= maxEntries {
			break
		}
		name := e.Name()
		// Refuse credential-bearing FILES outright (don't even list them).
		// Directory-level skip happens via skipDir below once we decide isDir.
		if !e.IsDir() && skipFile(name) {
			continue
		}
		*count++
		childRel := name
		if rel != "" {
			childRel = rel + "/" + name
		}
		// Never follow symlinks: a symlinked dir (e.g. .claude/skills/<bundle>)
		// would escape into the global catalog. Surface it as a leaf.
		isSymlink := e.Type()&os.ModeSymlink != 0
		isDir := e.IsDir() && !isSymlink
		n := &Node{Name: name, Path: childRel, IsDir: isDir}
		if isDir && depth < maxDepth && !skipDir(name) {
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
// the absolute path (lexical only; callers that read also EvalSymlinks-check).
// Shared with the other CRUD packages via safepath.
func resolveIn(root, rel string) (string, error) {
	return safepath.ResolveLexical(root, rel)
}

// File reads one workspace file for inline preview. It refuses symlinks and
// directories, caps the read at 1 MiB (setting Truncated), and base64-encodes
// non-text content (images, binaries) so the UI can render it via a data URL.
func File(botID, sessionKey, relPath string) (FileContent, error) {
	var fc FileContent
	// Refuse credential-bearing dotfiles at the door (Sec J2). A hand-crafted
	// File("..../.netrc") path would otherwise bypass the tree-level filter
	// (readDir skips listing these but the path resolver doesn't refuse them).
	if skipFile(filepath.Base(relPath)) {
		return fc, fmt.Errorf("path is a credential-bearing file: %q", relPath)
	}
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

	// openNoFollow refuses a final-component symlink at open time, closing
	// the TOCTOU window where an agent with Bash could swap the regular
	// file for a symlink between our Lstat above and the open below (round
	// 13 M2). Windows lacks O_NOFOLLOW, so the Windows shim falls back to
	// os.Open after the Lstat guard — symlinks are rare in the typical
	// Windows agent sandbox.
	f, err := openNoFollow(full)
	if err != nil {
		if errors.Is(err, errSymlinkOpen) {
			return fc, fmt.Errorf("refusing to read symlink: %q", relPath)
		}
		return fc, err
	}
	defer f.Close()
	// Read up to the larger (binary) cap + 1 sentinel byte; we decide the real cap
	// once isTextual classifies the content below. Size the buffer to the file when
	// it's smaller than the cap, so a small text file doesn't allocate 8 MiB.
	// io.ReadFull tolerates short reads (pipes/slow FS) and reports the byte count
	// via ErrUnexpectedEOF / EOF.
	bufLen := maxBinaryBytes + 1
	if sz := fi.Size(); sz >= 0 && sz < int64(bufLen) {
		bufLen = int(sz) + 1
	}
	buf := make([]byte, bufLen)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return fc, err
	}
	data := buf[:n]

	mime := mimeOf(full, data)
	textual := isTextual(mime, data)
	// Per-kind cap: text previews at 1 MiB, binary (image/PDF/…) at 8 MiB.
	limit := maxBinaryBytes
	if textual {
		limit = maxTextBytes
	}
	truncated := n > limit
	if truncated {
		data = data[:limit]
	}

	fc = FileContent{
		Path:      filepath.ToSlash(relPath),
		Mime:      mime,
		Kind:      kindOf(mime, textual),
		Truncated: truncated,
		Size:      fi.Size(),
	}
	if textual {
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
	".pdf": "application/pdf",
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

// kindOf is the single source of truth for how the UI renders a file, so the
// frontend consumes one field instead of re-deriving from mime/encoding. textual
// is isTextual's result (utf8 vs base64). Order matters: markdown/html and svg
// satisfy a broader bucket, so check the specific kinds first.
func kindOf(mime string, textual bool) string {
	switch {
	case mime == "text/markdown":
		return "markdown"
	case mime == "text/html":
		return "html"
	case mime == "application/pdf":
		return "pdf"
	case !textual && strings.HasPrefix(mime, "image/"):
		return "image"
	case textual:
		return "text"
	default:
		return "binary"
	}
}

// skipDir reports whether the workspace file tree should refuse to descend
// into the named child directory. Two reasons to skip:
//
//   - `.claude` — the per-bot CLI config dir. Always present, never
//     interesting in a workspace context, and contains skill bundles +
//     workflows that have their own UIs.
//   - common credential-bearing dotdirs — the agent has Bash + bypass
//     permissions and chooses its own files. If it writes `.aws/credentials`
//     or `~/.ssh/id_rsa` under its cwd (e.g. a `cp ~/.aws/credentials .`
//     during a research turn), the desktop file pane would expose those
//     contents to anyone who can screenshot the GUI. This is operator
//     self-exposure, not RCE, but a viewing pane shouldn't surface secrets
//     by default. Operators who specifically want to inspect a `.aws/` dir
//     can still `cat` it via the agent's tools.
//
// skipDir reports whether the workspace file tree should refuse to descend
// into the named child DIRECTORY. Use skipFile for credential-bearing
// FILES (round 10 Sec J2 — the prior skipDir list mixed file names like
// `.netrc` in but readDir only consulted it for directories, so files
// slipped through and were readable via File()).
// skipDir reports whether the workspace file tree should refuse to descend
// into the named child DIRECTORY. Comparison is case-INSENSITIVE — macOS
// APFS-default and Windows NTFS resolve `.AWS/` to `.aws/` on read but
// `os.ReadDir` returns the on-disk casing verbatim, so a case-sensitive
// switch would silently leak `cp ~/.aws .AWS` (round 11 Sec).
func skipDir(name string) bool {
	switch strings.ToLower(name) {
	case ".claude",
		// Cloud / SaaS credential stores. If the agent runs a Bash command
		// that copies any of these into its cwd (e.g. a research turn that
		// `cp ~/.aws/credentials .`), the desktop file pane would expose
		// the credential by default. Operator self-exposure, not RCE, but
		// the file viewer shouldn't surface secrets unless explicitly asked.
		".aws", ".azure", ".gcloud",
		".ssh", ".gnupg", ".gpg",
		".docker", ".kube", ".helm",
		".cloudflared", ".terraform.d",
		".cargo", ".m2", ".gradle",
		".snowsql", ".databricks", ".kaggle",
		".continue", ".ipython",
		".config":
		return true
	}
	return false
}

// skipFile reports whether the workspace file tree should refuse to LIST
// or READ the named file. Catches credential-bearing dotfiles that an
// agent's bash might copy into cwd (`cp ~/.netrc .`). Consulted by both
// readDir (so they don't appear in the tree) and File (so a hand-crafted
// path can't read them either).
//
// All matches are case-INSENSITIVE — macOS APFS-default and Windows NTFS
// preserve case on read but resolve case-insensitively, so a file landing
// as `.NETRC` or `Id_Rsa` would slip a case-sensitive list (round 11 Sec).
// Two match modes:
//   - exact basename (lowercase) — for canonical names like `.netrc`,
//     `authorized_keys`, `id_rsa` and its family
//   - dangerous extension — for cert/key file types whose names vary
//     widely (`server.key`, `mycert.pem`, `wallet.kdbx`)
func skipFile(name string) bool {
	lc := strings.ToLower(name)
	switch lc {
	case ".netrc", ".npmrc", ".pypirc",
		".git-credentials", ".pgpass",
		".my.cnf",
		"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
		"id_rsa.pub", "id_ecdsa.pub", "id_ed25519.pub",
		"authorized_keys", "known_hosts":
		return true
	}
	// `.env` / `.env.local` / `.env.production` …
	if lc == ".env" || strings.HasPrefix(lc, ".env.") {
		return true
	}
	// Common cert / key / keystore / password-store extensions.
	switch filepath.Ext(lc) {
	case ".pem", ".key", ".p12", ".pfx",
		".jks", ".keystore", ".kdbx",
		".kubeconfig", ".ovpn":
		return true
	}
	return false
}
