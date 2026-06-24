//go:build unix

package safepath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

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
		next, err := mkdirStep(cur, p, uint32(perm), strings.Join(parts[:i+1], "/"))
		if err != nil {
			return err
		}
		unix.Close(cur)
		cur = next
	}
	return nil
}

func mkdirStep(cur int, part string, perm uint32, logicalPath string) (int, error) {
	next, oerr := unix.Openat(cur, part, noFollowDirFlags, 0)
	if oerr == nil {
		return next, nil
	}
	if err := classifyMkdirOpenError(cur, part, oerr, logicalPath); err != nil {
		return -1, err
	}
	if err := mkdirMissingComponent(cur, part, perm); err != nil {
		return -1, err
	}
	next, oerr = unix.Openat(cur, part, noFollowDirFlags, 0)
	if oerr != nil {
		return -1, classifyOpenErr(cur, part, oerr, logicalPath)
	}
	return next, nil
}

func classifyMkdirOpenError(cur int, part string, oerr error, logicalPath string) error {
	// Component is either a symlink (classifyOpenErr translates), missing (then
	// we mkdir it), or genuinely failing.
	if isSymlinkErrno(oerr) {
		return pathErrSymlink(logicalPath)
	}
	// If fstatat says it's a symlink, the kernel may have returned ENOTDIR (e.g.
	// macOS with O_DIRECTORY|O_NOFOLLOW) — surface as ErrSymlink before treating
	// as missing. If it exists as a real directory, another goroutine raced us to
	// create it (concurrent SafeMkdirAll under the same parent): skip the mkdirat
	// and re-open rather than surfacing the now-stale ENOENT from our Openat.
	var st unix.Stat_t
	if serr := unix.Fstatat(cur, part, &st, unix.AT_SYMLINK_NOFOLLOW); serr == nil {
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return pathErrSymlink(logicalPath)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			// Exists but isn't a directory and isn't a symlink — surface the
			// genuine open error.
			return oerr
		}
		return nil
	} else if !errors.Is(oerr, unix.ENOENT) {
		return oerr
	}
	return nil
}

func mkdirMissingComponent(cur int, part string, perm uint32) error {
	// Component missing — mkdirat then re-open with O_NOFOLLOW.
	if merr := unix.Mkdirat(cur, part, perm); merr != nil {
		// Race-tolerant: another caller may have created the dir between our
		// openat and our mkdirat. EEXIST is fine as long as the now-existing
		// component is a directory (re-openat with O_DIRECTORY|O_NOFOLLOW will
		// succeed).
		if !errors.Is(merr, unix.EEXIST) {
			return merr
		}
	}
	return nil
}
