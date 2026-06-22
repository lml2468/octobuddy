//go:build windows

// Symlink-safe path operations, Windows fallback. Windows has no openat-
// family of syscalls (FILE_FLAG_OPEN_REPARSE_POINT via golang.org/x/sys/
// windows would be the structural equivalent but pulls in a sizable
// per-component CreateFileW rewrite), so this shim relies on Lstat-chain
// pre-checks at every component AND on the leaf, then performs the
// operation via the standard os package.
//
// Residual risk on Windows: an attacker who can race the Lstat-chain
// against the actual open can still slip a symlink through (TOCTOU window
// of single milliseconds). The Unix dirfd walk closes this structurally;
// the Windows fallback does not. Mitigations:
// - Symlink creation on Windows requires admin OR Developer Mode enabled,
// so the attacker surface is narrower than POSIX.
// - Per-component Lstat refusal still defeats the "drop a symlink and
// wait" attack (the most common shape) — the race window is sub-ms.
// - This file documents the gap; switching to FILE_FLAG_OPEN_REPARSE_POINT
// is the future close.

package safepath

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func lstatChain(root, rel string) (string, error) {
	abs, err := ResolveLexical(root, rel)
	if err != nil {
		return "", err
	}
	// Walk: root, then root/c1, root/c1/c2, … Each level refused if it's a
	// reparse point (Windows' equivalent of symlink/junction/mountpoint).
	if fi, err := os.Lstat(root); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", pathErrSymlink(root)
		}
	}
	parts := strings.Split(strings.Trim(filepath.ToSlash(rel), "/"), "/")
	cur := root
	for i, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if p == ".." {
			return "", fmt.Errorf("path contains .. segment: %q", rel)
		}
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// non-existent component below — ok for SafeWrite / SafeMkdirAll
				return abs, nil
			}
			return "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", pathErrSymlink(strings.Join(parts[:i+1], "/"))
		}
	}
	return abs, nil
}

func SafeOpen(root, rel string) (*os.File, error) {
	abs, err := lstatChain(root, rel)
	if err != nil {
		return nil, err
	}
	return os.Open(abs)
}

func SafeRead(root, rel string, cap int64) ([]byte, error) {
	f, err := SafeOpen(root, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if cap <= 0 {
		return io.ReadAll(f)
	}
	// Hard cap: error rather than silently truncate.
	buf, err := io.ReadAll(io.LimitReader(f, cap+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > cap {
		return nil, fmt.Errorf("safepath: file %q exceeds %d byte cap", rel, cap)
	}
	return buf, nil
}

func SafeWrite(root, rel string, data []byte, perm os.FileMode) error {
	abs, err := lstatChain(root, rel)
	if err != nil {
		return err
	}
	// Temp + fsync + rename for atomicity AND durability. The temp goes in
	// the same dir so rename stays on one filesystem. O_EXCL guards against
	// a collision with an unrelated tmp (8-byte hex makes this vanishingly
	// rare, but the contract is "atomic write"); Sync flushes the bytes to
	// disk before the rename publishes the inode — without it a power loss
	// between the rename and the FS journal flush would leave a zero-byte
	// cron.json / config.json. This matches the Unix branch's guarantees.
	dir := filepath.Dir(abs)
	var rb [8]byte
	if _, rerr := rand.Read(rb[:]); rerr != nil {
		return rerr
	}
	tmp := filepath.Join(dir, ".tmp."+filepath.Base(abs)+"."+hex.EncodeToString(rb[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, werr := f.Write(data); werr != nil {
		f.Close()
		_ = os.Remove(tmp)
		return werr
	}
	if serr := f.Sync(); serr != nil {
		f.Close()
		_ = os.Remove(tmp)
		return serr
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	if rerr := os.Rename(tmp, abs); rerr != nil {
		_ = os.Remove(tmp)
		return rerr
	}
	return nil
}

func SafeReadDir(root, rel string) ([]os.DirEntry, error) {
	abs, err := lstatChain(root, rel)
	if err != nil {
		return nil, err
	}
	return os.ReadDir(abs)
}

func SafeMkdirAll(root, rel string, perm os.FileMode) error {
	abs, err := lstatChain(root, rel)
	if err != nil {
		return err
	}
	return os.MkdirAll(abs, perm)
}

func SafeRemove(root, rel string) error {
	abs, err := lstatChain(root, rel)
	if err != nil {
		return err
	}
	return os.Remove(abs)
}

func SafeRemoveAll(root, rel string) error {
	abs, err := lstatChain(root, rel)
	if err != nil {
		return err
	}
	return os.RemoveAll(abs)
}

func SafeLstat(root, rel string) (os.FileInfo, error) {
	abs, err := lstatChain(root, rel)
	if err != nil {
		return nil, err
	}
	return os.Lstat(abs)
}

func SafeExists(root, rel string) bool {
	_, err := SafeLstat(root, rel)
	return err == nil
}
