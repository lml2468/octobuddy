// Package windowstate persists the console window's last known
// position+size so a relaunch restores it instead of always opening at
// the default 1040×720 top-left. macOS users expect window state memory;
// Linux/Windows users tolerate its absence but appreciate it.
//
// Storage: ~/.octobuddy/window.json (one JSON object). Bounded to a
// single small file so the package can use safepath.SafeReadAbs /
// SafeWriteAbs unconditionally (no symlink leaf attack surface).
//
// Failure mode: Load returns a zero State on any error (missing file,
// corrupted JSON, unreachable home dir). Save logs to stderr and
// swallows — window-state loss is a UX regression, not a correctness bug.
package windowstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lml2468/octobuddy/core/safepath"
	"github.com/lml2468/octobuddy/desktop/internal/configstore"
)

// State captures the bounds we care about restoring. Negative or zero
// values are interpreted as "use default" by the caller.
type State struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// IsZero reports whether the state carries no usable bounds (e.g. fresh
// install or a Load that fell back to the zero value).
func (s State) IsZero() bool {
	return s == State{}
}

// filePath returns ~/.octobuddy/window.json or "" when HOME is unset.
// Resolves the daemon-state directory through configstore.Dir() so the
// root layout has a single source of truth — if it ever moves (e.g.
// macOS Application Support), windowstate moves with it for free.
func filePath() (string, error) {
	dir := configstore.Dir()
	if dir == "" {
		return "", fmt.Errorf("windowstate: resolve home: %w", os.ErrNotExist)
	}
	return filepath.Join(dir, "window.json"), nil
}

// Load returns the persisted window state, or (zero, nil) when the file
// doesn't exist yet. Corrupted JSON is logged via the returned error and
// treated as "use default" by callers (they typically `state, _ := Load()`
// and check IsZero).
func Load() (State, error) {
	path, err := filePath()
	if err != nil {
		return State{}, err
	}
	data, err := safepath.SafeReadAbs(path, 4<<10) // 4 KiB is plenty for {x,y,w,h}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("windowstate: read: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("windowstate: decode: %w", err)
	}
	return s, nil
}

// Save persists the state to ~/.octobuddy/window.json. Creates the
// parent directory if needed. Best-effort — the caller is expected to
// log+swallow on error (window state loss is non-critical).
func Save(s State) error {
	path, err := filePath()
	if err != nil {
		return err
	}
	if err := safepath.SafeMkdirAllAbs(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("windowstate: mkdir: %w", err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("windowstate: encode: %w", err)
	}
	if err := safepath.SafeWriteAbs(path, data, 0o600); err != nil {
		return fmt.Errorf("windowstate: write: %w", err)
	}
	return nil
}
