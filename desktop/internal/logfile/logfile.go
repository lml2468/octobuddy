// Package logfile is a tiny rotating file writer for the desktop's combined
// log stream (its own log.Print output + the daemon's stdout/stderr forwarded
// by Supervisor). It exists so end users can `cat ~/.xclaw/logs/xclaw.log` or
// share it with support after a "出错了" — without it, the daemon's gateway
// error lines vanish into /dev/null whenever the app is launched normally
// (instead of from a terminal that holds stderr open).
//
// Rotation policy is deliberately minimal: append, and when the live file
// exceeds maxBytes, rename it to <name>.1 (deleting any prior .1) and reopen.
// One backup, not a ring — keeps the on-disk footprint bounded at ~2x maxBytes
// with no time-window logic to get wrong. Concurrent writers are serialized by
// a single mutex; throughput is fine for human-readable log lines.
package logfile

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Writer is an io.WriteCloser that appends to a single log file and rotates
// it once when the size cap is exceeded. Safe for concurrent use.
type Writer struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	f        *os.File
	size     int64
}

// New opens (or creates) dir/name for append and returns a Writer. dir is
// created with 0o755 if missing. maxBytes is the rotation threshold; pass 0
// to disable rotation entirely (the file grows unbounded).
//
// The returned Writer SHOULD have its Close called on shutdown but is safe to
// leak: the kernel flushes pending writes when the process exits.
func New(dir, name string, maxBytes int64) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("logfile: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("logfile: open %s: %w", path, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("logfile: stat %s: %w", path, err)
	}
	return &Writer{path: path, maxBytes: maxBytes, f: f, size: fi.Size()}, nil
}

// Path returns the absolute path of the live log file. Stable across rotations
// (the rotated copy gets the .1 suffix; this Writer always writes to the same
// path). Useful for the "查看日志" tray menu and similar consumers.
func (w *Writer) Path() string { return w.path }

// Write appends p, rotating first if the post-write size would exceed the cap.
// Rotation atomically renames the live file to <path>.1 (replacing any prior
// .1) and reopens a fresh empty file at the original path, so external tail -F
// stays attached across rotations (inode changes but the path is reopened
// fresh — tail's -F handles this; -f would not).
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.maxBytes > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			// Rotation failure shouldn't lose the write. Fall through and append
			// to whatever file we have open. The size cap may be exceeded by
			// one window's worth of writes until rotation succeeds next time.
			fmt.Fprintf(os.Stderr, "logfile: rotate failed (will retry): %v\n", err)
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// Close releases the underlying file. Subsequent Writes return an error.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// rotateLocked closes the live file, renames it to <path>.1 (replacing any
// existing .1), and reopens a fresh empty file at <path>. Caller MUST hold mu.
func (w *Writer) rotateLocked() error {
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("close live: %w", err)
	}
	rotated := w.path + ".1"
	_ = os.Remove(rotated) // ignore "doesn't exist"; Rename below would fail anyway if for some other reason
	if err := os.Rename(w.path, rotated); err != nil {
		// Couldn't move out of the way — reopen the original to keep writing.
		// Better to blow past the cap than to drop writes.
		f, openErr := os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if openErr != nil {
			return fmt.Errorf("rename %s → %s failed (%v) AND reopen failed: %w", w.path, rotated, err, openErr)
		}
		w.f = f
		// size unchanged
		return fmt.Errorf("rename %s → %s: %w", w.path, rotated, err)
	}
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("reopen %s after rotate: %w", w.path, err)
	}
	w.f = f
	w.size = 0
	return nil
}

// Tee returns an io.Writer that writes to both this Writer and `other`. Use it
// to keep the desktop's own log going to os.Stderr (visible when launched from
// a terminal) while ALSO persisting to disk. Thin wrapper around io.MultiWriter
// kept here so callers don't have to import both packages.
func (w *Writer) Tee(other io.Writer) io.Writer { return io.MultiWriter(w, other) }
