//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package workspace

import (
	"errors"
	"syscall"
)

// isNoFollowSymlinkErrno reports whether err is the BSD kernel's "open of a
// symlink with O_NOFOLLOW" signal. Different BSDs use different errnos —
// Darwin returns ELOOP on recent kernels but historically returned EFTYPE;
// FreeBSD returns EMLINK; NetBSD/OpenBSD return EFTYPE. Match all three so
// the user-visible "refusing to read symlink" message is consistent across
// macOS / FreeBSD / NetBSD / OpenBSD / Dragonfly (round 14 G #3).
func isNoFollowSymlinkErrno(err error) bool {
	return errors.Is(err, syscall.ELOOP) ||
		errors.Is(err, syscall.EFTYPE) ||
		errors.Is(err, syscall.EMLINK)
}
