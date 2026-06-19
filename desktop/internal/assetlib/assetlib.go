// Package assetlib holds the per-bot "install" primitives shared by the skills
// and workflows packages. An install is a symlink in a bot's own asset dir
// (~/.xclaw/<id>/skills|workflows) pointing at a read-only marketplace catalog
// entry; the bot may also keep its own real (non-symlink) assets alongside.
//
// Skills are directory bundles and workflows are single .js files, but the
// install/uninstall/prune mechanics are identical, so they live here once rather
// than being copy-pasted into each package.
package assetlib

import (
	"fmt"
	"os"
	"path/filepath"
)

// IsSymlink reports whether path is a symlink. A missing path is not a symlink
// (ok=false, err=nil); any other lstat error is returned.
func IsSymlink(path string) (ok bool, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return info.Mode()&os.ModeSymlink != 0, nil
}

// Install symlinks dst → src (src must be an absolute catalog path the caller has
// already validated exists). Idempotent: a symlink already pointing at src is
// left as-is; a symlink with a stale target is replaced; a REAL (non-symlink)
// entry at dst is refused so a bot's own asset is never clobbered. label names
// the asset kind for the error message ("skill" / "workflow").
func Install(src, dst, label string) error {
	if info, err := os.Lstat(dst); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if cur, _ := os.Readlink(dst); cur == src {
				return nil // already installed, correct target
			}
			_ = os.Remove(dst) // stale target → replace
		} else {
			return fmt.Errorf("a per-bot %s named %q already exists", label, filepath.Base(dst))
		}
	}
	return os.Symlink(src, dst)
}

// Uninstall removes dst only when it is a symlink (an installed catalog asset),
// so a bot's own real asset is never deleted. Absent → nil (idempotent). A real
// entry → error telling the caller to delete it instead. label names the kind.
func Uninstall(dst, label string) error {
	info, err := os.Lstat(dst)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%s %q is a per-bot asset, not an installed one — delete it instead", label, filepath.Base(dst))
	}
	return os.Remove(dst)
}

// Prune removes path iff it is a symlink, and reports whether it removed one.
// A real entry or a missing path is left untouched (no error) — used when a
// catalog entry is deleted, to clear the now-dangling install symlinks in every
// bot without ever touching a bot's own same-named asset.
func Prune(path string) (removed bool, err error) {
	link, err := IsSymlink(path)
	if err != nil {
		return false, err
	}
	if !link {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return true, nil
}

// PruneInstallsAcrossBots clears the now-dangling install symlinks for a catalog
// entry that was just deleted. It scans the per-bot asset dirs (<xclawDir>/<id>/
// <subdir>) and Prunes <linkName> in each — only symlinks are removed, so a bot's
// own real asset of the same name survives. catalogSubdirs are the install-wide
// dirs (e.g. "skills", "workflows", "bin") to skip while scanning for bot ids.
// Best-effort: per-bot errors are ignored so one unreadable bot can't block the
// catalog delete.
func PruneInstallsAcrossBots(xclawDir, subdir, linkName string, catalogSubdirs ...string) {
	entries, err := os.ReadDir(xclawDir)
	if err != nil {
		return
	}
	skip := map[string]bool{}
	for _, s := range catalogSubdirs {
		skip[s] = true
	}
	for _, e := range entries {
		id := e.Name()
		if !e.IsDir() || skip[id] {
			continue
		}
		_, _ = Prune(filepath.Join(xclawDir, id, subdir, linkName))
	}
}

