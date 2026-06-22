//go:build windows

package config

import (
	"fmt"
	"io"
	"os"
)

// readNoFollow on Windows pre-checks via Lstat and refuses if the leaf is
// a reparse point (symlink / junction / mount point). Race window between
// Lstat and Open remains; Windows symlink creation requires admin or
// Developer Mode so the agent's attack surface is narrower than POSIX.
func readNoFollow(path string) ([]byte, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to follow symlink: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1<<20))
}
