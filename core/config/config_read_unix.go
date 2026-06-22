//go:build unix

package config

import (
	"io"
	"os"
	"syscall"
)

// readNoFollow reads up to 1 MiB from path, refusing to follow a symlink at
// the leaf. Used by soul() for SOUL.md / AGENTS.md so an agent that plants
// `~/.xclaw/<id>/SOUL.md → /Users/victim/.aws/credentials` cannot redirect
// the operator-trusted system-prompt source.
func readNoFollow(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1<<20))
}
