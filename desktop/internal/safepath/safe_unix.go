//go:build unix

// Symlink-safe path operations via dirfd walk. The structural guarantee:
// every path component is opened with O_NOFOLLOW|O_DIRECTORY relative to its
// parent dirfd, so a symlink anywhere returns ELOOP (Linux) / EFTYPE / EMLINK
// (BSDs) and the walk aborts. The final operation (read/write/list/remove)
// runs against the verified dirfd via openat/renameat/unlinkat — the kernel
// never re-traverses the absolute path that an attacker could have swapped
// between our walk and our use. This is the same pattern container runtimes
// use for path resolution under untrusted roots.
//
// Linux 5.6+ offers openat2(RESOLVE_NO_SYMLINKS|RESOLVE_BENEATH) which would
// collapse the whole walk to one syscall, but the dirfd walk is portable to
// macOS / FreeBSD / NetBSD without a per-OS branch — kept for simplicity until
// a performance need motivates the optimization.

package safepath

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// noFollowDirFlags opens a directory refusing symlinks at the leaf.
// O_CLOEXEC keeps the FD from leaking to child processes.
const noFollowDirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// isSymlinkErrno is defined per-OS — Linux returns ELOOP, BSDs add EFTYPE /
// EMLINK depending on kernel — but the shared callsites all just ask
// "did the kernel refuse this because the leaf is a symlink?".

// walkToDir returns an *os.File for <root>/<rel> as a directory, having
// refused a symlink at every component (root included). rel may be empty
// (returns an FD for root itself). Callers MUST Close the returned file.
func walkToDir(root, rel string) (*os.File, error) {
	rel = filepath.ToSlash(rel)
	rootFD, err := unix.Open(root, noFollowDirFlags, 0)
	if err != nil {
		return nil, classifyOpenErr(0, root, err, root)
	}
	cur := rootFD
	curName := root
	if rel == "" || rel == "." {
		return os.NewFile(uintptr(cur), curName), nil
	}
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	for i, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if p == ".." {
			unix.Close(cur)
			return nil, fmt.Errorf("path contains .. segment: %q", rel)
		}
		next, oerr := unix.Openat(cur, p, noFollowDirFlags, 0)
		if oerr != nil {
			cerr := classifyOpenErr(cur, p, oerr, strings.Join(parts[:i+1], "/"))
			unix.Close(cur)
			return nil, cerr
		}
		unix.Close(cur)
		cur = next
		curName = filepath.Join(curName, p)
	}
	return os.NewFile(uintptr(cur), curName), nil
}

// classifyOpenErr translates an openat failure into ErrSymlink when the
// component is actually a symlink — even when the errno is something else
// (notably ENOTDIR, which kernels prefer over ELOOP when O_DIRECTORY is set
// and the leaf is a symlink-to-anything, e.g. macOS). A non-symlink failure
// (genuine ENOTDIR for a regular file used as a parent, ENOENT, EACCES, …)
// bubbles up verbatim so callers' error messages stay useful.
// dirfd may be 0 to indicate "open the leaf as an absolute path with Lstat";
// otherwise fstatat(dirfd, name) is used.
func classifyOpenErr(dirfd int, name string, openErr error, displayPath string) error {
	if isSymlinkErrno(openErr) {
		return pathErrSymlink(displayPath)
	}
	var st unix.Stat_t
	var serr error
	if dirfd == 0 {
		serr = unix.Lstat(name, &st)
	} else {
		serr = unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	}
	if serr == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return pathErrSymlink(displayPath)
	}
	return openErr
}

// splitLeaf returns (parentRel, leaf). For "a/b/c.txt" → ("a/b", "c.txt").
// For a single-segment "c.txt" → ("", "c.txt"). Errors on empty leaf.
func splitLeaf(rel string) (string, string, error) {
	rel = strings.Trim(filepath.ToSlash(rel), "/")
	if rel == "" {
		return "", "", fmt.Errorf("empty path")
	}
	i := strings.LastIndex(rel, "/")
	if i < 0 {
		return "", rel, nil
	}
	leaf := rel[i+1:]
	if leaf == "" {
		return "", "", fmt.Errorf("path has no leaf: %q", rel)
	}
	return rel[:i], leaf, nil
}

// SafeOpen opens <root>/<rel> read-only with structural symlink refusal at
// every component. The returned *os.File is guaranteed to be a regular file
// reached without traversing any symlink. Caller MUST Close.
//
// Round 19 Go #2: opens with O_NONBLOCK and post-fstats to reject FIFOs,
// devices, sockets — an agent with Bash in its sandbox can mkfifo a file
// in workspace/, then a click in the file-preview pane would otherwise
// block the desktop's Wails IPC handler forever waiting for a writer.
func SafeOpen(root, rel string) (*os.File, error) {
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
	fd, err := unix.Openat(int(parent.Fd()), leaf, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, classifyOpenErr(int(parent.Fd()), leaf, err, rel)
	}
	f := os.NewFile(uintptr(fd), filepath.Join(root, rel))
	// Reject anything that isn't a regular file BEFORE handing the FD to
	// the caller (who will likely io.ReadAll it — that's where a FIFO
	// reader would block). Then clear O_NONBLOCK so the regular-file read
	// uses the normal blocking path (regular files don't actually block,
	// so this is purely a hygiene step).
	var st unix.Stat_t
	if serr := unix.Fstat(int(f.Fd()), &st); serr != nil {
		f.Close()
		return nil, serr
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		f.Close()
		return nil, fmt.Errorf("safepath: not a regular file: %q", rel)
	}
	// Clear O_NONBLOCK now that we know it's a regular file (no-op
	// behavior-wise but keeps the FD's flags in their "normal" state for
	// any io.Reader that introspects them).
	if flags, ferr := unix.FcntlInt(uintptr(f.Fd()), unix.F_GETFL, 0); ferr == nil {
		_, _ = unix.FcntlInt(uintptr(f.Fd()), unix.F_SETFL, flags&^unix.O_NONBLOCK)
	}
	return f, nil
}

// SafeRead is SafeOpen + ReadAll. Cap is enforced via io.LimitReader; pass 0
// to read without a cap.
func SafeRead(root, rel string, cap int64) ([]byte, error) {
	f, err := SafeOpen(root, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if cap > 0 {
		return io.ReadAll(io.LimitReader(f, cap))
	}
	return io.ReadAll(f)
}

// SafeWrite atomically writes data to <root>/<rel>. The parent chain is
// verified (no symlinks); the leaf is written to a temp inside the verified
// parent dirfd and renamed into place. Two structural guarantees:
//
//   - Path safety: openat/renameat on a dirfd never re-traverses an absolute
//     path, so an attacker who swaps a parent to a symlink between our walk
//     and our rename cannot redirect the write.
//   - Symlink-leaf refusal: if the destination already exists as a symlink,
//     the renameat WOULD silently replace it with our regular file. We
//     fstatat the leaf first and refuse with ErrSymlink so the operator
//     learns about the tampering instead of having the symlink quietly
//     disappear. (Refusing also matches the SafeOpen behavior — symmetry.)
func SafeWrite(root, rel string, data []byte, perm os.FileMode) error {
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

	// Refuse to write through a leaf symlink (round-15+ invariant). AT_SYMLINK_
	// NOFOLLOW makes fstatat report the symlink itself rather than its target.
	var st unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), leaf, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return pathErrSymlink(rel)
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}

	tmpName, err := randomTmpName(leaf)
	if err != nil {
		return err
	}
	// O_CREAT|O_EXCL so a same-name pre-create races us cleanly (we error
	// out instead of clobbering); O_NOFOLLOW for symmetry with the rest of
	// the walk; perm sanitized by the kernel's umask.
	tmpFD, err := unix.Openat(int(parent.Fd()), tmpName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm))
	if err != nil {
		return err
	}
	tmp := os.NewFile(uintptr(tmpFD), tmpName)
	if _, werr := tmp.Write(data); werr != nil {
		tmp.Close()
		_ = unix.Unlinkat(int(parent.Fd()), tmpName, 0)
		return werr
	}
	if serr := tmp.Sync(); serr != nil {
		tmp.Close()
		_ = unix.Unlinkat(int(parent.Fd()), tmpName, 0)
		return serr
	}
	if cerr := tmp.Close(); cerr != nil {
		_ = unix.Unlinkat(int(parent.Fd()), tmpName, 0)
		return cerr
	}
	// Renameat replaces the destination atomically; both ends use the same
	// verified dirfd so neither path is re-traversed via the VFS.
	if rerr := unix.Renameat(int(parent.Fd()), tmpName, int(parent.Fd()), leaf); rerr != nil {
		_ = unix.Unlinkat(int(parent.Fd()), tmpName, 0)
		return rerr
	}
	return nil
}

// SafeReadDir lists entries directly under <root>/<rel> (rel may be empty for
// the root itself). Returns the raw os.DirEntry slice — callers choose how to
// handle symlink entries (some want them as leaves, others want them skipped).
// Sub-trees are NOT walked; callers recurse via further SafeReadDir calls.
func SafeReadDir(root, rel string) ([]os.DirEntry, error) {
	if rel != "" {
		if _, err := ResolveLexical(root, rel); err != nil {
			return nil, err
		}
	}
	dir, err := walkToDir(root, rel)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return dir.ReadDir(-1)
}

// SafeMkdirAll creates the directory <root>/<rel> and any missing parents,
// refusing to traverse OR create through a symlink. Each existing component
// is opened with O_NOFOLLOW|O_DIRECTORY (symlink → ErrSymlink); each missing
// component is mkdirat'd into the verified parent dirfd.
func SafeMkdirAll(root, rel string, perm os.FileMode) error {
	if rel == "" || rel == "." {
		return nil
	}
	if _, err := ResolveLexical(root, rel); err != nil {
		return err
	}
	rootFD, err := unix.Open(root, noFollowDirFlags, 0)
	if err != nil {
		return classifyOpenErr(0, root, err, root)
	}
	cur := rootFD
	defer func() { unix.Close(cur) }()
	parts := strings.Split(strings.Trim(filepath.ToSlash(rel), "/"), "/")
	for i, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if p == ".." {
			return fmt.Errorf("path contains .. segment: %q", rel)
		}
		next, oerr := unix.Openat(cur, p, noFollowDirFlags, 0)
		if oerr == nil {
			unix.Close(cur)
			cur = next
			continue
		}
		// Component is either a symlink (classifyOpenErr translates),
		// missing (then we mkdir it), or genuinely failing.
		if isSymlinkErrno(oerr) {
			return pathErrSymlink(strings.Join(parts[:i+1], "/"))
		}
		// If fstatat says it's a symlink, the kernel may have returned
		// ENOTDIR (e.g. macOS with O_DIRECTORY|O_NOFOLLOW) — surface as
		// ErrSymlink before treating as missing.
		var st unix.Stat_t
		if serr := unix.Fstatat(cur, p, &st, unix.AT_SYMLINK_NOFOLLOW); serr == nil {
			if st.Mode&unix.S_IFMT == unix.S_IFLNK {
				return pathErrSymlink(strings.Join(parts[:i+1], "/"))
			}
			// Exists but isn't a directory and isn't a symlink — surface
			// the genuine open error.
			return oerr
		}
		if !errors.Is(oerr, unix.ENOENT) {
			return oerr
		}
		// Component missing — mkdirat then re-open with O_NOFOLLOW.
		if merr := unix.Mkdirat(cur, p, uint32(perm)); merr != nil {
			return merr
		}
		next, oerr = unix.Openat(cur, p, noFollowDirFlags, 0)
		if oerr != nil {
			return oerr
		}
		unix.Close(cur)
		cur = next
	}
	return nil
}

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
	} else if !errors.Is(err, unix.EISDIR) && !errors.Is(err, unix.EPERM) {
		// EPERM on directories on some systems (Linux). ENOENT means already gone.
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		// fall through to dir-handling below
	}
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
