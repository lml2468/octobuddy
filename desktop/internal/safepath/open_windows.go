//go:build windows

package safepath

import (
	"errors"
	"os"
)

// ErrSymlinkLeaf parity on Windows — there's no O_NOFOLLOW analogue, so the
// fallback opens normally after an Lstat refusal of a symlink leaf. Round 17
// Arch #1: the prior version delegated symlink-refusal entirely to callers
// (each had to remember a separate Lstat), and skills/workflows callers
// dropped it after the round-16 OpenNoFollow refactor. Fold the Lstat guard
// into the shim itself so every caller is uniformly safe on Windows too —
// the real-path TOCTOU window is wider than Unix's O_NOFOLLOW (FILE_FLAG_OPEN_
// REPARSE_POINT would close it but needs golang.org/x/sys/windows), but a
// guarded fallback beats a silent symlink-follow.
var ErrSymlinkLeaf = errors.New("safepath: refusing to follow symlink at open")

// isSymlink reports whether path's final component is a reparse point /
// symlink. Returns false when Lstat fails (the caller's own Open then errors
// with the underlying cause). Reparse points on Windows include real
// symlinks, junctions, and mount points — all of which we treat as
// "do not follow".
func isSymlink(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

func OpenNoFollow(path string) (*os.File, error) {
	if isSymlink(path) {
		return nil, ErrSymlinkLeaf
	}
	return os.Open(path)
}

func WriteNoFollow(path string, data []byte, perm os.FileMode) error {
	if isSymlink(path) {
		return ErrSymlinkLeaf
	}
	return os.WriteFile(path, data, perm)
}
