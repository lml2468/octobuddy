package workflows

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCRUDAndValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := Create("deploy"); err != nil {
		t.Fatal(err)
	}
	if err := Create("deploy"); err == nil {
		t.Error("duplicate create should error")
	}
	got, _ := Read("deploy")
	if got == "" || !filepathHasExt(filepath.Join(Dir(), "deploy.js")) {
		t.Fatalf("read/scaffold failed: %q", got)
	}
	if err := Write("deploy", "export const meta={name:'deploy',description:'ship it'}\n"); err != nil {
		t.Fatal(err)
	}
	list, _ := List()
	if len(list) != 1 || list[0].Name != "deploy" || list[0].Description != "ship it" {
		t.Fatalf("list = %+v", list)
	}
	for _, bad := range []string{"../evil", "a/b", "", "..", "x.js/y"} {
		if err := Create(bad); err == nil {
			t.Errorf("invalid name %q should be rejected", bad)
		}
	}
	if err := Delete("deploy"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(Dir(), "deploy.js")); !os.IsNotExist(err) {
		t.Error("workflow should be gone after Delete")
	}
}

func filepathHasExt(p string) bool { _, err := os.Stat(p); return err == nil }
