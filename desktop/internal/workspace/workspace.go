// Package workspace exposes read-only views of a chat session's sandbox-backed
// files to the desktop app: workspace files under
// ~/.octobuddy/<botID>/workspace/<hash> and auto-memory files under
// ~/.octobuddy/<botID>/memory/<hash>. The hash is the same one core/sandbox derives
// from the session's (kind, sessionKey), and callers pass the persisted channel
// type explicitly so the file browser does not guess DM vs group.
//
// Everything here is read-only and defensive: bot IDs are slug-validated,
// per-file paths are containment-checked (mirroring internal/skills),
// symlinks are never followed (G #3 added an O_NOFOLLOW open
// for the final component on Unix), and the walk is bounded in depth,
// entry count, and file size.
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

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/safepath"
	"github.com/lml2468/octobuddy/core/sandbox"
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

const (
	channelDM      = 1
	channelGroup   = 2
	channelConsole = 3
)

type treeSpace string

const (
	workspaceSpace treeSpace = "workspace"
	memorySpace    treeSpace = "memory"
)

// Dir is ~/.octobuddy (the install root), matching configstore.Dir.
func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".octobuddy")
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

func kindForChannelType(channelType int) (sandbox.Kind, error) {
	switch channelType {
	case channelDM, channelConsole:
		return sandbox.KindDM, nil
	case channelGroup:
		return sandbox.KindGroup, nil
	default:
		return "", fmt.Errorf("unsupported channel type %d", channelType)
	}
}

// resolveRoot returns the session's file-root directory and whether it exists.
// When neither exists yet (no turn has run or the agent has not written memory),
// exists is false and dir is still the deterministic candidate path.
func resolveRoot(space treeSpace, botID string, channelType int, sessionKey string) (dir string, exists bool, err error) {
	if !safepath.ValidSlug(botID) {
		return "", false, fmt.Errorf("invalid bot id %q", botID)
	}
	kind, err := kindForChannelType(channelType)
	if err != nil {
		return "", false, err
	}
	base := filepath.Join(Dir(), botID, string(space))
	dir = filepath.Join(base, sandbox.SessionDirName(sandbox.SessionCtx{Kind: kind, SessionKey: sessionKey}))
	if fi, e := os.Stat(dir); e == nil && fi.IsDir() {
		return dir, true, nil
	}
	return dir, false, nil
}

// Tree returns the session's workspace as a bounded file tree. When no sandbox
// exists yet (no turn has run), it returns an empty (non-nil) root so the UI can
// show an "empty workspace" state without an error.
func Tree(botID string, channelType int, sessionKey string) (*Node, error) {
	return tree(workspaceSpace, botID, channelType, sessionKey)
}

// MemoryTree returns the session's auto-memory directory as a bounded file tree.
// Missing memory is rendered as an empty non-nil root, matching Tree.
func MemoryTree(botID string, channelType int, sessionKey string) (*Node, error) {
	return tree(memorySpace, botID, channelType, sessionKey)
}

func tree(space treeSpace, botID string, channelType int, sessionKey string) (*Node, error) {
	root, exists, err := resolveRoot(space, botID, channelType, sessionKey)
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

func readDir(root, rel string, depth int, count *int) ([]*Node, error) {
	entries, err := safepath.SafeReadDir(root, rel)
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
	// mean "not expandable" (depth-cap /.claude / symlink leaf); without
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
		// skip symlinked entries entirely instead of
		// surfacing them as bogus leaves. Clicking one would invoke
		// File → SafeOpen → ErrSymlink ("refusing to read symlink") —
		// click-to-error noise that no legitimate workflow produces.
		// Matches the skills/workflows listing policy: symlinks under
		// these agent-writable dirs are tampering signals, not content.
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		*count++
		childRel := name
		if rel != "" {
			childRel = rel + "/" + name
		}
		n := &Node{Name: name, Path: childRel, IsDir: e.IsDir()}
		if e.IsDir() && depth < maxDepth && !skipDir(name) {
			kids, err := readDir(root, childRel, depth+1, count)
			if err == nil {
				n.Children = kids
			}
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// File reads one workspace file for inline preview. It refuses symlinks and
// directories, caps the read at 1 MiB (setting Truncated), and base64-encodes
// non-text content (images, binaries) so the UI can render it via a data URL.
// All path safety — lexical containment, parent-chain symlink refusal,
// race-free leaf open — lives in safepath; this function has no Lstat /
// EvalSymlinks / O_NOFOLLOW concerns of its own.
func File(botID string, channelType int, sessionKey, relPath string) (FileContent, error) {
	return file(workspaceSpace, botID, channelType, sessionKey, relPath)
}

// MemoryFile reads one file from the session's auto-memory directory.
func MemoryFile(botID string, channelType int, sessionKey, relPath string) (FileContent, error) {
	return file(memorySpace, botID, channelType, sessionKey, relPath)
}

func file(space treeSpace, botID string, channelType int, sessionKey, relPath string) (FileContent, error) {
	var fc FileContent
	// Refuse credential-bearing dotfiles at the door. A hand-crafted
	// File("..../.netrc") path would otherwise bypass the tree-level filter
	// (readDir skips listing these but the path resolver doesn't refuse them).
	if skipFile(filepath.Base(relPath)) {
		return fc, fmt.Errorf("path is a credential-bearing file: %q", relPath)
	}
	// Tree refuses to descend into skipDir entries (.aws,
	//.ssh,.kube, …), so the user never sees them in the file picker — but
	// File relied only on basename matching, so a hand-crafted relPath
	// like ".aws/credentials" passed every check and read the file. Walk
	// every path segment through skipDir to close that door too.
	for _, seg := range strings.Split(filepath.ToSlash(relPath), "/") {
		if seg != "" && skipDir(seg) {
			return fc, fmt.Errorf("path traverses a credential-bearing directory: %q", relPath)
		}
	}
	root, exists, err := resolveRoot(space, botID, channelType, sessionKey)
	if err != nil {
		return fc, err
	}
	if !exists {
		return fc, fmt.Errorf("no %s yet for this session", space)
	}

	// One call does it all: lexical containment, parent-chain symlink walk
	// via dirfd, leaf O_NOFOLLOW open. The returned FD is guaranteed to be
	// reached without traversing any symlink, race-free against an agent
	// swapping parents mid-open.
	f, err := safepath.SafeOpen(root, relPath)
	if err != nil {
		if errors.Is(err, safepath.ErrSymlink) {
			return fc, fmt.Errorf("refusing to read symlink: %q", relPath)
		}
		return fc, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fc, err
	}
	if fi.IsDir() {
		return fc, fmt.Errorf("not a file: %q", relPath)
	}

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

	mime := mimeOf(relPath, data)
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

// agentConfigDirs is the set of per-bot agent CLI config-dir names to skip,
// derived ONCE from the driver registry (Claude ".claude", Codex ".codex", …)
// rather than hardcoded — so a newly-registered driver's config dir is skipped
// automatically instead of silently leaking into the file pane until someone
// remembers to extend a literal switch. Lowercased for the case-insensitive
// match in skipDir. ".claude" is always included as a floor in case a driver
// reports no config dir.
var agentConfigDirs = func() map[string]bool {
	m := map[string]bool{".claude": true}
	for _, name := range agent.Registered() {
		if d := agent.ConfigDirNameFor(name); d != "" {
			m[strings.ToLower(d)] = true
		}
	}
	return m
}()

// skipDir reports whether the workspace file tree should refuse to descend
// into the named child DIRECTORY. Two reasons to skip:
//
// - per-bot agent CLI config dirs (`.claude` / `.codex` / …, from the driver
// registry via agentConfigDirs). Always present for the bot's driver, never
// interesting in a workspace context, and contain skill bundles + workflows
// that have their own UIs.
// - common credential-bearing dotdirs — the agent has Bash + bypass
// permissions and chooses its own files. If it writes `.aws/credentials`
// or `~/.ssh/id_rsa` under its cwd (e.g. a `cp ~/.aws/credentials.`
// during a research turn), the desktop file pane would expose those
// contents to anyone who can screenshot the GUI. This is operator
// self-exposure, not RCE, but a viewing pane shouldn't surface secrets
// by default. Operators who specifically want to inspect a `.aws/` dir
// can still `cat` it via the agent's tools.
//
// File also walks every path segment of a hand-crafted relPath through
// this list, so a request for `.aws/credentials` is
// refused even though Tree never lists the parent.
//
// Comparison is case-INSENSITIVE — macOS APFS-default and Windows NTFS
// resolve `.AWS/` to `.aws/` on read but `os.ReadDir` returns the on-disk
// casing verbatim, so a case-sensitive switch would silently leak
// `cp ~/.aws.AWS` (Sec). Use skipFile for credential-bearing
// FILES.
func skipDir(name string) bool {
	lower := strings.ToLower(name)
	if agentConfigDirs[lower] {
		return true
	}
	switch lower {
	case
		// Cloud / SaaS credential stores. If the agent runs a Bash command
		// that copies any of these into its cwd (e.g. a research turn that
		// `cp ~/.aws/credentials.`), the desktop file pane would expose
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
// agent's bash might copy into cwd (`cp ~/.netrc.`). Consulted by both
// readDir (so they don't appear in the tree) and File (so a hand-crafted
// path can't read them either).
//
// All matches are case-INSENSITIVE — macOS APFS-default and Windows NTFS
// preserve case on read but resolve case-insensitively, so a file landing
// as `.NETRC` or `Id_Rsa` would slip a case-sensitive list (Sec).
// Two match modes:
// - exact basename (lowercase) — for canonical names like `.netrc`,
// `authorized_keys`, `id_rsa` and its family
// - dangerous extension — for cert/key file types whose names vary
// widely (`server.key`, `mycert.pem`, `wallet.kdbx`)
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
