package assetlib

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallIdempotentAndGuards(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "catalog", "alpha")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(base, "bot", "alpha")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Install(src, dst, "skill"); err != nil {
		t.Fatal(err)
	}
	if l, _ := IsSymlink(dst); !l {
		t.Fatal("install should create a symlink")
	}
	// Idempotent.
	if err := Install(src, dst, "skill"); err != nil {
		t.Errorf("re-install should be idempotent: %v", err)
	}
	// A stale symlink target is replaced.
	other := filepath.Join(base, "catalog", "beta")
	_ = os.MkdirAll(other, 0o755)
	_ = os.Remove(dst)
	_ = os.Symlink(other, dst)
	if err := Install(src, dst, "skill"); err != nil {
		t.Fatal(err)
	}
	if tgt, _ := os.Readlink(dst); tgt != src {
		t.Errorf("stale symlink should be repointed: %q want %q", tgt, src)
	}
	// A real entry is never clobbered.
	real := filepath.Join(base, "bot", "own")
	_ = os.MkdirAll(real, 0o755)
	if err := Install(src, real, "skill"); err == nil {
		t.Error("install must refuse to overwrite a real per-bot asset")
	}
}

func TestUninstallAndPruneOnlyTouchSymlinks(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "catalog", "alpha")
	_ = os.MkdirAll(src, 0o755)

	link := filepath.Join(base, "bot", "alpha")
	_ = os.MkdirAll(filepath.Dir(link), 0o755)
	_ = os.Symlink(src, link)
	real := filepath.Join(base, "bot", "own")
	_ = os.MkdirAll(real, 0o755)

	// Uninstall removes the symlink, refuses the real asset.
	if err := Uninstall(link, "skill"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Error("symlink should be gone after uninstall")
	}
	if err := Uninstall(real, "skill"); err == nil {
		t.Error("uninstall must refuse a real per-bot asset")
	}
	// Uninstall on a missing path is a no-op.
	if err := Uninstall(filepath.Join(base, "bot", "ghost"), "skill"); err != nil {
		t.Errorf("uninstall of missing path should be nil: %v", err)
	}

	// Prune removes only symlinks.
	_ = os.Symlink(src, link)
	if rm, err := Prune(link); err != nil || !rm {
		t.Errorf("Prune should remove a symlink: rm=%v err=%v", rm, err)
	}
	if rm, err := Prune(real); err != nil || rm {
		t.Errorf("Prune must not touch a real asset: rm=%v err=%v", rm, err)
	}
}

func TestPruneInstallsAcrossBots(t *testing.T) {
	xclaw := t.TempDir()
	catalog := filepath.Join(xclaw, "skills", "alpha")
	_ = os.MkdirAll(catalog, 0o755)

	// bot1 installed it (symlink); bot2 has a real same-named asset.
	b1 := filepath.Join(xclaw, "bot1", "skills")
	_ = os.MkdirAll(b1, 0o755)
	_ = os.Symlink(catalog, filepath.Join(b1, "alpha"))
	b2 := filepath.Join(xclaw, "bot2", "skills", "alpha")
	_ = os.MkdirAll(b2, 0o755)

	PruneInstallsAcrossBots(xclaw, "skills", "alpha", "skills", "workflows", "bin")

	if _, err := os.Lstat(filepath.Join(b1, "alpha")); !os.IsNotExist(err) {
		t.Error("bot1 install symlink should be pruned")
	}
	if _, err := os.Stat(b2); err != nil {
		t.Error("bot2 real asset must survive prune")
	}
}
