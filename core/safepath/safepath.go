// Package safepath is the single source of truth for path safety in the
// desktop's local-file CRUD packages (skills, workflows, workspace,
// configstore). It enforces three invariants in one place so callers can
// stop worrying about them:
//
// 1. Slug validation: per-component names must match a strict character
// class (ValidSlug). The caller's `name` arg is checked once at the
// boundary; afterwards it can't contain a path separator or "..".
//
// 2. Containment: the resolved absolute path must lie inside `root`.
// Enforced lexically by ResolveLexical (no FS touch) AND structurally
// by the Safe* file ops below (every path component walked with
// O_NOFOLLOW so an intermediate symlink can't redirect).
//
// 3. Symlink refusal: every Safe* op refuses to traverse OR overwrite a
// symlink at any path component. Race-free on Unix via dirfd-walk
// (openat with O_NOFOLLOW per component, then operations on the
// verified dirfd via openat/renameat/unlinkat — the kernel never
// re-traverses an absolute path that an attacker could have swapped
// between our check and our use). On Windows, an Lstat-chain
// fallback; structural openat-equivalents need x/sys/windows
// reparse-point handling that's not implemented here. Windows
// residual: a same-uid attacker with Developer Mode CAN race the
// Lstat-then-open window; documented but unmitigated.
//
// Callers should NEVER do their own os.Lstat / filepath.EvalSymlinks /
// O_NOFOLLOW / symlink-mode checks for paths under a `root`. Those are
// this package's responsibility; sprinkling them at callsites is what
// led to the prior versions incremental defenses we collapsed here.
package safepath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var slugRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// UsableExecutable reports whether fi (typically from SafeLstat) describes a
// real, runnable binary file: a non-symlink regular file of non-zero size, and
// (on unix only — Windows doesn't gate on +x) with at least one execute bit set.
// Shared by every "resolve the managed claude binary, else fall back to PATH"
// site (the daemon's resolveClaudeBin and the desktop's claudecli.isFile) so the
// usability predicate can't drift between them. Pair it with SafeLstat (which
// already refuses a symlinked PARENT component); this checks the leaf's mode.
func UsableExecutable(fi os.FileInfo) bool {
	if fi == nil || fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || fi.Size() == 0 {
		return false
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
		return false
	}
	return true
}

// ValidSlug reports whether s is a safe single path segment: non-empty, not "."
// or "..", does not begin with "." (no dotfile collisions inside ~/.octobuddy/),
// and only letters/digits/dot/underscore/dash (so it can't contain a path
// separator or traversal).
func ValidSlug(s string) bool {
	if s == "" || s == "." || s == ".." || strings.HasPrefix(s, ".") {
		return false
	}
	return slugRe.MatchString(s)
}

// ResolveLexical validates that rel is a clean relative path inside root and
// returns the absolute path WITHOUT touching the filesystem. It rejects empty,
// absolute, and any ".." segment outright (rather than silently rewriting), with
// a final lexical containment check. Use this when the target may not exist yet
// AND you don't need symlink safety (e.g. computing a display path). For
// anything that opens / reads / writes / lists a path, use the Safe* ops below;
// they call this internally as a pre-filter and then add the symlink-safe walk.
func ResolveLexical(root, rel string) (string, error) {
	rel = filepath.ToSlash(rel)
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("absolute path not allowed: %q", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path escapes root: %q", rel)
		}
	}
	// Clean the root so the containment compare is consistent with `full` (which
	// filepath.Join cleans) regardless of how the caller spelled the root — and so
	// the path-separator is OS-native on Windows (filepath.Join uses backslashes
	// there, which a raw forward-slash root would not prefix-match).
	root = filepath.Clean(root)
	full := filepath.Join(root, filepath.FromSlash(rel))
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes root: %q", rel)
	}
	return full, nil
}

// ErrSymlink is the sentinel returned (wrapped) by Safe* ops when they refuse
// to traverse or overwrite a symlink anywhere on the path. Callers can use
// errors.Is(err, safepath.ErrSymlink) to detect tampering and surface a
// distinct user-facing message; everything else (not-found, permission, etc.)
// bubbles up as the underlying os error.
var ErrSymlink = errors.New("safepath: refusing to follow symlink")

// pathErrSymlink wraps ErrSymlink with the path that tripped it, so error
// messages identify WHERE the tampering was detected without leaking the
// (possibly attacker-influenced) symlink target.
func pathErrSymlink(rel string) error {
	return fmt.Errorf("%w: %q", ErrSymlink, rel)
}
