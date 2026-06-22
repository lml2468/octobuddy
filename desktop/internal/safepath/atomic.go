// Package-level symlink-safe atomic write. Lives at the package root rather
// than per-platform shims because the safety + atomicity invariants compose
// the same way everywhere — we just delegate to atomicfile.Write after a
// race-free symlink refusal at the destination leaf.

package safepath

import (
	"errors"
	"os"

	"github.com/lml2468/xclaw/core/atomicfile"
)

// AtomicWriteNoFollow refuses to overwrite a symlink at the leaf and then
// writes data via atomicfile.Write (temp + fsync + rename). Combines two
// invariants the per-bot file CRUD paths both need (round 17 Go #4 — drift
// vs round 15's atomicfile routing for SOUL/AGENTS):
//
//   - symlink refusal: an agent-planted `bundle/SKILL.md → ~/.zshrc` MUST
//     NOT be writable through. The pre-write Lstat is the same shim
//     pattern WriteNoFollow uses on Windows, and on Unix it's belt-and-
//     suspenders alongside the atomicfile temp file's own O_EXCL (the
//     temp filename is unpredictable, so an attacker can't plant a
//     symlink at the destination of os.Rename either — rename atomically
//     replaces whatever's there).
//   - crash safety: a kill -9 during the write leaves either the old
//     file intact or the fully committed new one — never a partial mix.
//
// Returns ErrSymlinkLeaf when the destination already exists as a symlink.
func AtomicWriteNoFollow(path string, data []byte, perm os.FileMode) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return ErrSymlinkLeaf
	} else if err != nil && !os.IsNotExist(err) {
		// Bubble up anything other than "doesn't exist yet" so the
		// caller doesn't blindly try to write into an unreadable path.
		return err
	}
	if err := atomicfile.Write(path, data, perm); err != nil {
		// atomicfile errors don't include ErrSymlinkLeaf — wrap is unnecessary.
		return err
	}
	return nil
}

// IsErrSymlinkLeaf is the canonical predicate for "refused because of a leaf
// symlink"; provided so callers can check without importing errors directly
// just for this one Is() call.
func IsErrSymlinkLeaf(err error) bool { return errors.Is(err, ErrSymlinkLeaf) }
