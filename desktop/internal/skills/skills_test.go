package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setup(t *testing.T) { t.Helper(); t.Setenv("HOME", t.TempDir()) }

func TestCreateListFilesRoundTrip(t *testing.T) {
	setup(t)
	if err := Create("demo"); err != nil {
		t.Fatal(err)
	}
	if err := Create("demo"); err == nil {
		t.Error("creating an existing skill should error")
	}
	if err := WriteFile("demo", "scripts/run.sh", "#!/bin/sh\necho hi\n"); err != nil {
		t.Fatal(err)
	}
	files, err := Files("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 { // SKILL.md + scripts/run.sh
		t.Fatalf("files = %v, want 2", files)
	}
	got, _ := ReadFile("demo", "scripts/run.sh")
	if !strings.Contains(got, "echo hi") {
		t.Errorf("read back %q", got)
	}
	list, _ := List()
	if len(list) != 1 || list[0].Name != "demo" || list[0].Files != 2 {
		t.Fatalf("list = %+v", list)
	}
	if list[0].Description == "" {
		t.Errorf("scaffolded SKILL.md should yield a description")
	}
	if err := DeleteFile("demo", "scripts/run.sh"); err != nil {
		t.Fatal(err)
	}
	if err := Delete("demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(Dir(), "demo")); !os.IsNotExist(err) {
		t.Error("skill dir should be gone after Delete")
	}
}

func TestPathTraversalRejected(t *testing.T) {
	setup(t)
	_ = Create("demo")
	// Plant a secret outside the skill dir; ensure it can't be read/written via ...
	outside := filepath.Join(Dir(), "..", "secret.txt")
	_ = os.WriteFile(outside, []byte("TOPSECRET"), 0o644)

	for _, rel := range []string{"../secret.txt", "../../secret.txt", "/etc/passwd", "a/../../secret.txt"} {
		if _, err := ReadFile("demo", rel); err == nil {
			t.Errorf("ReadFile(%q) should be rejected", rel)
		}
		if err := WriteFile("demo", rel, "x"); err == nil {
			t.Errorf("WriteFile(%q) should be rejected", rel)
		}
	}
	// the outside secret must be untouched
	if b, _ := os.ReadFile(outside); string(b) != "TOPSECRET" {
		t.Error("path traversal modified a file outside the skill dir")
	}
	// invalid skill names rejected
	if err := Create("../evil"); err == nil {
		t.Error("invalid skill name should be rejected")
	}
}
