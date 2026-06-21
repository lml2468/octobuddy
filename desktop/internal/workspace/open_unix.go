//go:build unix

package workspace

import (
	"errors"
	"os"
	"syscall"
)

// errSymlinkOpen is returned by openNoFollow when the final path component is
// a symlink. The package's File() maps this to a stable error message.
var errSymlinkOpen = errors.New("workspace: refusing to follow symlink at open")

// openNoFollow opens path with O_NOFOLLOW and maps the platform-specific
// "is a symlink" errno to errSymlinkOpen. Linux returns ELOOP; the BSDs
// (Darwin/FreeBSD/NetBSD) historically return EFTYPE or EMLINK depending
// on kernel version, so we widen the match (round 14 G #3). Any other
// open error bubbles up verbatim so the caller's "no such file" / EACCES
// messages stay useful.
func openNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil && isNoFollowSymlinkErrno(err) {
		return nil, errSymlinkOpen
	}
	return f, err
}
