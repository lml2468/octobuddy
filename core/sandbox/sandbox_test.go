package sandbox

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
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

func TestResolveSessionCwdIdempotent(t *testing.T) {
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
	// Idempotent: a second resolve returns the same dir without error (sandboxes
	// are persistent — no TTL reclamation, no marker).
	dir2, err := ResolveSessionCwd(base, ctx)
	if err != nil || dir2 != dir {
		t.Fatalf("second resolve = (%q, %v), want (%q, nil)", dir2, err, dir)
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
