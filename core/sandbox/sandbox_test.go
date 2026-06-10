package sandbox

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

var hexName = regexp.MustCompile(`^[0-9a-f]{16}$`)

func TestHashDeterministicAndKindScoped(t *testing.T) {
	dm := SessionCtx{Kind: KindDM, SessionKey: "x"}
	grp := SessionCtx{Kind: KindGroup, SessionKey: "x"}

	if hashKey(dm.partitionKey()) != hashKey(dm.partitionKey()) {
		t.Fatal("hash not deterministic")
	}
	if hashKey(dm.partitionKey()) == hashKey(grp.partitionKey()) {
		t.Fatal("kind must scope the hash: dm:x and group:x collided")
	}
	if !hexName.MatchString(hashKey(dm.partitionKey())) {
		t.Fatalf("hash not 16-hex: %q", hashKey(dm.partitionKey()))
	}
	if hashKey("a") == hashKey("b") {
		t.Fatal("distinct keys collided")
	}
}

func TestResolveSessionCwdIdempotentWithMarker(t *testing.T) {
	base := t.TempDir()
	ctx := SessionCtx{Kind: KindDM, SessionKey: "u1"}

	dir, err := ResolveSessionCwd(base, ctx)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := filepath.Join(base, hashKey(ctx.partitionKey()))
	if dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("sandbox dir not created: %v", err)
	}
	marker := filepath.Join(base, registryDirName, hashKey(ctx.partitionKey()))
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("registry marker missing: %v", err)
	}

	// Idempotent + mtime refresh: backdate, resolve again, mtime advances.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveSessionCwd(base, ctx); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	st, _ := os.Stat(dir)
	if !st.ModTime().After(old.Add(time.Hour)) {
		t.Fatal("mtime not refreshed on re-resolve")
	}
}

func TestResolveMemoryDirIsPure(t *testing.T) {
	base := t.TempDir()
	memBase := filepath.Join(base, "memory")
	ctx := SessionCtx{Kind: KindGroup, SessionKey: "c1"}

	mem := ResolveMemoryDir(memBase, ctx)
	want := filepath.Join(memBase, hashKey(ctx.partitionKey()))
	if mem != want {
		t.Fatalf("mem = %q, want %q", mem, want)
	}
	// Pure: must NOT create anything on disk.
	if _, err := os.Stat(mem); !os.IsNotExist(err) {
		t.Fatalf("ResolveMemoryDir must not create the dir; stat err = %v", err)
	}
	// Same partition key as cwd → same hash component.
	cwd, _ := ResolveSessionCwd(base, ctx)
	if filepath.Base(cwd) != filepath.Base(mem) {
		t.Fatalf("cwd and memory must share the hash: %q vs %q", filepath.Base(cwd), filepath.Base(mem))
	}
}

func TestCleanupExpiredCwds(t *testing.T) {
	base := t.TempDir()
	keep, _ := ResolveSessionCwd(base, SessionCtx{Kind: KindDM, SessionKey: "fresh"})
	drop, _ := ResolveSessionCwd(base, SessionCtx{Kind: KindDM, SessionKey: "stale"})

	// Backdate "drop" past the TTL (dir + its registry marker mtime not relevant;
	// only the dir mtime gates deletion).
	old := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(drop, old, old); err != nil {
		t.Fatal(err)
	}

	CleanupExpiredCwds(base, 7*24*time.Hour)

	if _, err := os.Stat(drop); !os.IsNotExist(err) {
		t.Fatal("stale sandbox should have been swept")
	}
	if _, err := os.Stat(filepath.Join(base, registryDirName, filepath.Base(drop))); !os.IsNotExist(err) {
		t.Fatal("stale marker should have been removed too")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("fresh sandbox must survive")
	}
}

func TestCleanupRegistryGuard(t *testing.T) {
	base := t.TempDir()
	// A 16-hex dir we did NOT create (no marker) — must never be swept, any age.
	foreign := filepath.Join(base, "0123456789abcdef")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-100 * 24 * time.Hour)
	_ = os.Chtimes(foreign, old, old)
	// A non-hex dir — also never touched.
	other := filepath.Join(base, "not-a-session")
	_ = os.MkdirAll(other, 0o755)
	_ = os.Chtimes(other, old, old)

	CleanupExpiredCwds(base, time.Hour)

	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("foreign hex dir without marker must not be deleted (P0-3)")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("non-hex dir must never be touched")
	}
}

func TestCleanupMissingBaseNoPanic(t *testing.T) {
	CleanupExpiredCwds(filepath.Join(t.TempDir(), "nope"), time.Hour) // must not panic
}
