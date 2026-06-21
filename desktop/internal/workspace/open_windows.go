//go:build windows

package workspace

import (
	"errors"
	"os"
)

// errSymlinkOpen is the cross-platform sentinel; Windows can't actually emit
// it because there's no O_NOFOLLOW analogue here. The Lstat guard at the
// caller still refuses symlinks; this open is the unprotected fallback.
var errSymlinkOpen = errors.New("workspace: refusing to follow symlink at open")

func openNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
