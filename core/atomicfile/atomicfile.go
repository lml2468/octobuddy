// Package atomicfile is a dependency-free leaf both core and desktop use for
// crash-safe file writes: temp + fsync + rename, with the temp removed on any
// failure between create and rename. Lifted out of core/cron and
// desktop/internal/configstore (which carried byte-identical copies) so the
// pattern lives in one place — the desktop-vs-core duplication of this and
// related I/O policy was the root cause of `configstore.go` churning in four
// of five audit rounds.
package atomicfile

import (
	"fmt"
	"os"
	"sync/atomic"
)

// tmpCounter monotonically increments to disambiguate concurrent writes within
// the same process so two callers don't open the same .tmp path with TRUNC and
// silently clobber each other's bytes (round 14 G #5).
var tmpCounter uint64

// Write writes data to path via a per-call unique temp file in path's directory
// + Sync + Rename so a power loss or process crash mid-write leaves either the
// old file intact or a fully committed new file — never a half-written one,
// and never a partial mix of two concurrent writers' bytes. The temp is
// removed on any failure between create and rename so stale temps don't
// accumulate.
//
// Limitation: parent-dir fsync is omitted. POSIX rename durability requires
// fsync(dirfd) after the rename; without it, a power loss between rename and
// the next dirent flush can resurrect the old file. This is acceptable for
// XClaw — config.json / cron.json crash-recoverability is bounded by the
// last operator action, not transactional — and matches the prevailing
// industry pattern for application-level atomic writes.
func Write(path string, data []byte, perm os.FileMode) (err error) {
	// Per-call unique suffix + O_EXCL so two concurrent Write callers (e.g.
	// SaveConfig racing a skill CRUD on a sibling file in the same dir)
	// never share a tmp name. Previously a fixed path+".tmp" with O_TRUNC
	// let the loser silently overwrite the winner's partial bytes.
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), atomic.AddUint64(&tmpCounter, 1))
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
