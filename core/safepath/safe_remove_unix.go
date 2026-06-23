//go:build unix

package safepath

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// SafeRemove unlinks a single file at <root>/<rel>. Refuses to traverse a
// symlink at any path component. To delete a symlink ENTRY itself (rather
// than its target), this is a no-op: SafeLstat will surface the symlink so
// callers can decide; the dedicated SafeRemoveSymlink is exposed for the
// rare "clean up tampering evidence" case.
func SafeRemove(root, rel string) error {
	if _, err := ResolveLexical(root, rel); err != nil {
		return err
	}
	parentRel, leaf, err := splitLeaf(rel)
	if err != nil {
		return err
	}
	parent, err := walkToDir(root, parentRel)
	if err != nil {
		return err
	}
	defer parent.Close()
	return unix.Unlinkat(int(parent.Fd()), leaf, 0)
}

// SafeRemoveAll recursively removes the file or directory at <root>/<rel>.
// Refuses to traverse a symlink at any component, AND refuses to descend
// into a symlinked subdirectory (it unlinks the symlink itself rather
// than following it — same policy as os.RemoveAll). The dirfd walk to
// the parent makes the operation race-free against parent-component
// symlink swaps; within the target subtree, the walk uses dirfds at each
// level so an attacker swapping a sub-component mid-delete is detected.
func SafeRemoveAll(root, rel string) error {
	if _, err := ResolveLexical(root, rel); err != nil {
		return err
	}
	parentRel, leaf, err := splitLeaf(rel)
	if err != nil {
		return err
	}
	parent, err := walkToDir(root, parentRel)
	if err != nil {
		return err
	}
	defer parent.Close()
	return removeAllAt(int(parent.Fd()), leaf)
}

// removeAllAt unlinks `name` relative to dirfd. If `name` is a directory,
// recurses into it via openat(O_NOFOLLOW|O_DIRECTORY) and unlinks contents
// before rmdir-ing the directory itself. Symlink entries inside are
// unlinked (not followed), matching os.RemoveAll's policy.
func removeAllAt(dirfd int, name string) error {
	// Try unlink first — works for files and symlinks, fast path.
	if err := unix.Unlinkat(dirfd, name, 0); err == nil {
		return nil
	} else if errors.Is(err, unix.ENOENT) {
		// Already gone — idempotent success.
		return nil
	} else if !errors.Is(err, unix.EISDIR) && !errors.Is(err, unix.EPERM) {
		// Genuine error on a non-directory (EACCES on a read-only mount,
		// EROFS, EIO, chattr +i, …) — surface it. Falling through to the
		// dir-handling branch below misclassified these as ENOTDIR (the
		// Openat(O_DIRECTORY|O_NOFOLLOW) on a regular file returns
		// ENOTDIR) and discarded the original errno.
		return &os.PathError{Op: "unlinkat", Path: name, Err: err}
	}
	// EISDIR / EPERM on Linux: the entry is a directory.
	// Open as dir with O_NOFOLLOW: a symlink entry can't slip into the
	// recursive descent.
	sub, err := unix.Openat(dirfd, name, noFollowDirFlags, 0)
	if err != nil {
		if isSymlinkErrno(err) {
			// Shouldn't reach here normally — symlinks unlink via the first
			// Unlinkat above. Defensive: still refuse to descend.
			return pathErrSymlink(name)
		}
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	dir := os.NewFile(uintptr(sub), name)
	entries, derr := dir.ReadDir(-1)
	if derr != nil {
		dir.Close()
		return derr
	}
	for _, e := range entries {
		if cerr := removeAllAt(int(dir.Fd()), e.Name()); cerr != nil {
			dir.Close()
			return cerr
		}
	}
	dir.Close()
	return unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR)
}

// SafeLstat returns Lstat-equivalent info for <root>/<rel> after verifying
// the parent chain has no symlinks. The leaf itself MAY be a symlink — the
// caller learns this from FileInfo.Mode and decides what to do.
func SafeLstat(root, rel string) (os.FileInfo, error) {
	if _, err := ResolveLexical(root, rel); err != nil {
		return nil, err
	}
	parentRel, leaf, err := splitLeaf(rel)
	if err != nil {
		return nil, err
	}
	parent, err := walkToDir(root, parentRel)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	var st unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), leaf, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	return fileInfoFromStat(&st, leaf), nil
}

// SafeExists is a convenience: SafeLstat + IsNotExist check, treating any
// non-not-found error as "exists" (operator should investigate separately).
func SafeExists(root, rel string) bool {
	_, err := SafeLstat(root, rel)
	return err == nil
}
