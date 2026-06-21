//go:build linux

package workspace

import (
	"errors"
	"syscall"
)

// isNoFollowSymlinkErrno reports whether err is the kernel's "open of a
// symlink with O_NOFOLLOW" signal. Linux returns ELOOP unconditionally.
func isNoFollowSymlinkErrno(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}
