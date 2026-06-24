package autostart

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/lml2468/octobuddy/desktop/internal/desktest"
)

func TestEnabledOnEmptyHomeIsFalse(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only")
	}
	desktest.WithHome(t, t.TempDir())
	ok, err := Enabled()
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("clean HOME must report Enabled=false")
	}
}

func TestPlistPathFollowsHome(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only")
	}
	home := t.TempDir()
	desktest.WithHome(t, home)
	want := filepath.Join(home, "Library", "LaunchAgents", "com.mlt.octobuddy.desktop.plist")
	if got := plistPath(); got != want {
		t.Fatalf("plistPath = %q, want %q", got, want)
	}
}

func TestDisableIdempotentOnAbsentPlist(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only")
	}
	desktest.WithHome(t, t.TempDir())
	if err := Disable(); err != nil {
		t.Fatalf("Disable on a clean HOME should be a no-op, got %v", err)
	}
}

func TestEnableRefusesOutsideBundle(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only")
	}
	// `go test` runs the test binary from a temp dir that is NOT inside a
	//.app/Contents/MacOS layout, so Enable must refuse with the bundle-path
	// error — and must NOT have written a plist or run launchctl.
	desktest.WithHome(t, t.TempDir())
	err := Enable()
	if err == nil {
		t.Fatal("Enable from a non-bundle path must error")
	}
	if _, err := os.Stat(plistPath()); !os.IsNotExist(err) {
		t.Fatalf("failed Enable must not leave a plist on disk: %v", err)
	}
}

func TestSupportedMatchesGOOS(t *testing.T) {
	got := Supported()
	want := runtime.GOOS == "darwin"
	if got != want {
		t.Fatalf("Supported() = %v on %s, want %v", got, runtime.GOOS, want)
	}
}
