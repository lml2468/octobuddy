// Package desktest holds shared test helpers for desktop/internal/* packages.
//
// Kept under desktop/internal so it never ships in a production binary
// (Go won't compile internal packages outside their module subtree, and
// the file is _-only callers).
package desktest

import "testing"

// WithHome redirects os.UserHomeDir into dir for the test's lifetime.
// Sets BOTH HOME (POSIX) and USERPROFILE (Windows) so the same helper
// resolves correctly on every supported CI runner — desktop tests that
// only set HOME silently break on windows-latest, which is the bug this
// helper exists to prevent. t.Setenv handles cleanup.
func WithHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}
