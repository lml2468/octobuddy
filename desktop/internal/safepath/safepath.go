// Package safepath centralizes the slug + path-traversal + symlink-containment
// checks the desktop's local-file CRUD packages (skills, workflows, workspace,
// configstore) all rely on. Keeping them in one place stops the defenses from
// drifting apart — previously each package reimplemented them and only workspace
// had grown the symlink-realpath check, leaving skills/workflows weaker.
package safepath

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var slugRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidSlug reports whether s is a safe single path segment: non-empty, not "."
// or "..", and only letters/digits/dot/underscore/dash (so it can't contain a
// path separator or traversal).
func ValidSlug(s string) bool {
	return s != "" && s != "." && s != ".." && slugRe.MatchString(s)
}

// ResolveLexical validates that rel is a clean relative path inside root and
// returns the absolute path WITHOUT touching the filesystem. It rejects empty,
// absolute, and any ".." segment outright (rather than silently rewriting), with
// a final lexical containment check. Use this when the target may not exist yet.
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

// AssertNoSymlinkEscape verifies, in real-path space, that full resolves to a
// location inside root — defending against an intermediate symlinked component
// that lexical checks miss (an agent with Bash could plant a symlink inside the
// catalog, since the catalog is reachable via absolute paths). dirOnly resolves
// the PARENT of full when full itself may not exist yet (a create), so the symlink
// check still covers every existing ancestor directory.
//
// Returns nil when neither root nor the resolved path can be symlink-resolved
// because they don't exist yet AND no existing ancestor escapes — i.e. a brand
// new tree under a real root is allowed.
func AssertNoSymlinkEscape(root, full string, dirOnly bool) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		// Root doesn't exist yet (first write into a fresh catalog dir): fall back
		// to lexical containment, already enforced by ResolveLexical's caller.
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	target := full
	if dirOnly {
		target = filepath.Dir(full)
	}
	real, err := resolveExistingPrefix(target)
	if err != nil {
		return err
	}
	if real != realRoot && !strings.HasPrefix(real, realRoot+string(os.PathSeparator)) {
		return fmt.Errorf("path escapes root via symlink: %q", full)
	}
	return nil
}

// resolveExistingPrefix EvalSymlinks the longest existing ancestor of p, then
// re-appends the non-existent tail lexically. This lets us symlink-check a path
// whose final components don't exist yet (a create) while still resolving every
// real (and possibly symlinked) ancestor directory.
func resolveExistingPrefix(p string) (string, error) {
	p = filepath.Clean(p)
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p, nil // reached the root; nothing more to resolve
	}
	realParent, err := resolveExistingPrefix(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, filepath.Base(p)), nil
}
