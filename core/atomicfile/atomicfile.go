// Package atomicfile is a dependency-free leaf both core and desktop use for
// crash-safe file writes: temp + fsync + rename, with the temp removed on any
// failure between create and rename. Lifted out of core/cron and
// desktop/internal/configstore (which carried byte-identical copies) so the
// pattern lives in one place — the desktop-vs-core duplication of this and
// related I/O policy was the root cause of `configstore.go` churning in four
// of five audit rounds.
package atomicfile

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// tmpCounter monotonically increments to disambiguate concurrent writes within
// the same process so two callers don't open the same .tmp path with TRUNC and
// silently clobber each other's bytes (round 14 G #5).
var tmpCounter uint64

// orphanTmpAge is how old a sibling .tmp must be before Write treats it as
// abandoned from a prior crash and removes it. Generous so a slow concurrent
// writer in another process can't be mistaken for an orphan.
const orphanTmpAge = 24 * time.Hour

// reapInterval throttles the orphan-reap dir scan: at most one scan per
// process per hour, no matter how often Write is called. Without this, every
// write (e.g. cron.json after each task fire) did an O(dir-entries) ReadDir
// in the hot path, AND an agent with bash that planted millions of
// `<base>.tmp.junk` siblings could DoS subsequent writes (round 16 H3).
// `lastReapNS` is the unix-nano timestamp of the last reap attempt, accessed
// only via atomics so reap-eligibility is racy-but-cheap (extra reap is
// harmless; missed reap recovers next hour).
const reapInterval = time.Hour

var lastReapNS atomic.Int64

// shouldReap returns true if at least reapInterval has elapsed since the last
// attempt AND atomically claims the slot for this caller. Returns false if
// another goroutine raced and won.
func shouldReap(now time.Time) bool {
	prior := lastReapNS.Load()
	if now.UnixNano()-prior < int64(reapInterval) {
		return false
	}
	return lastReapNS.CompareAndSwap(prior, now.UnixNano())
}

// randSuffix returns a short hex token from crypto/rand so the tmp filename
// is unpredictable to a local-read attacker (round 15 Sec M2). Without this,
// the tmp name was `path.tmp.<pid>.<counter>` — pid is observable via /proc
// or ps, counter starts at 1 each daemon launch, so an attacker with local
// write to the parent dir could pre-create a few sequential tmps and DoS
// SaveConfig via O_EXCL EEXIST. With 8 random bytes the namespace is 2^64
// and pre-creation becomes infeasible.
func randSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand essentially never fails on supported OSes; if it
		// does, fall back to the counter alone (still better than a
		// literal ".tmp").
		return ""
	}
	return hex.EncodeToString(b[:])
}

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
	// Best-effort: reap orphan tmps from a prior crashed write before we
	// add our own (round 15 Arch #2). The pre-round-15 implementation
	// reused a single `path.tmp` which was self-healing — the next
	// successful Write recreated it. The round-14 unique naming made tmps
	// accumulate forever if a kill -9 hit between create and rename, so
	// here we collect siblings whose names match our prefix-pattern AND
	// whose mtime is older than orphanTmpAge (24h). Failure to scan never
	// blocks the real write.
	//
	// Round 16 H3: gated behind shouldReap (at most once per hour per
	// process) AND capped at maxReapScan entries so an agent that planted
	// millions of `<base>.tmp.junk` siblings in an agent-writable dir
	// (e.g. ~/.xclaw/<id>/.claude/skills/<bundle>/) can't turn every
	// operator-side save into an O(N) stall.
	const maxReapScan = 512
	now := time.Now()
	if shouldReap(now) {
		tmpPrefix := filepath.Base(path) + ".tmp."
		if entries, derr := os.ReadDir(filepath.Dir(path)); derr == nil {
			cutoff := now.Add(-orphanTmpAge)
			scanned := 0
			for _, e := range entries {
				if !strings.HasPrefix(e.Name(), tmpPrefix) {
					continue
				}
				// Count only tmp-prefix candidates against the cap (round
				// 17 R17-M1): incrementing scanned BEFORE the prefix
				// check let an agent that planted hundreds of non-matching
				// decoys ahead of a real orphan exhaust the budget before
				// the orphan was even examined, INVERTING the DoS-
				// mitigation into "never reap actual evidence".
				scanned++
				if scanned > maxReapScan {
					break
				}
				info, ierr := e.Info()
				if ierr != nil || info.ModTime().After(cutoff) {
					continue
				}
				_ = os.Remove(filepath.Join(filepath.Dir(path), e.Name()))
			}
		}
	}

	// Per-call unique suffix + O_EXCL so two concurrent Write callers (e.g.
	// SaveConfig racing a skill CRUD on a sibling file in the same dir)
	// never share a tmp name. The crypto-rand component (round 15 Sec M2)
	// keeps the name unpredictable to a local-write attacker who could
	// otherwise pre-create the predictable `<pid>.<counter>` form to DoS
	// the save.
	tmp := fmt.Sprintf("%s.tmp.%d.%d.%s", path, os.Getpid(), atomic.AddUint64(&tmpCounter, 1), randSuffix())
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
