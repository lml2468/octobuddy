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

func openNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil && errors.Is(err, syscall.ELOOP) {
		return nil, errSymlinkOpen
	}
	return f, err
}
