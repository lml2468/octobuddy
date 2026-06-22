//go:build unix

package safepath

import (
	"errors"
	"os"
	"syscall"
)

// ErrSymlinkLeaf is returned when an open refuses to follow a symlink at the
// final path component. Callers translate this to a stable user-facing message.
var ErrSymlinkLeaf = errors.New("safepath: refusing to follow symlink at open")

// OpenNoFollow opens path read-only with O_NOFOLLOW. If the final path component
// is a symlink, returns ErrSymlinkLeaf (mapped from the kernel's platform-
// specific errno). All other errors bubble up verbatim so callers' "no such
// file" / EACCES messages stay useful. Round 16: factored out of the
// workspace package so skills/workflows write+read paths can use the same
// race-free open and close their leaf-symlink TOCTOU windows.
func OpenNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil && isSymlinkErrno(err) {
		return nil, ErrSymlinkLeaf
	}
	return f, err
}

// WriteNoFollow writes data to path via a fresh O_WRONLY|O_CREATE|O_NOFOLLOW
// open with the given perm. Refuses to overwrite a symlink at the leaf — that
// is the whole point: an attacker with bash on the same host could otherwise
// plant `bundle/SKILL.md → ~/.ssh/authorized_keys` and have the operator's
// next GUI save clobber the target. Returns ErrSymlinkLeaf in that case.
//
// Not atomic on its own. For crash-safety (a kill -9 between O_TRUNC and the
// final byte leaves a partial file) callers SHOULD route through
// safepath.AtomicWriteNoFollow below, which combines the symlink refusal
// with atomicfile's temp+rename. This bare helper is for the rare case
// where the caller needs the open semantics without atomicity (none today).
func WriteNoFollow(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, perm)
	if err != nil {
		if isSymlinkErrno(err) {
			return ErrSymlinkLeaf
		}
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
