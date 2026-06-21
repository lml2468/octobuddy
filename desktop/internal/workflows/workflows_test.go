package workflows

import (
	"os"
	"path/filepath"
	"testing"
)

func setHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

func botPath(t *testing.T, id string) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xclaw", id, ".claude", "workflows")
}

func TestBotCRUDAndValidation(t *testing.T) {
	setHome(t)
	if err := BotCreate("bot1", "deploy"); err != nil {
		t.Fatal(err)
	}
	if err := BotCreate("bot1", "deploy"); err == nil {
		t.Error("duplicate create should error")
	}
	got, _ := BotRead("bot1", "deploy")
	if got == "" {
		t.Fatalf("scaffold/read empty: %q", got)
	}
	if _, err := os.Stat(filepath.Join(botPath(t, "bot1"), "deploy.js")); err != nil {
		t.Fatalf("scaffold did not land on disk: %v", err)
	}
	if err := BotWrite("bot1", "deploy", "export const meta={name:'deploy',description:'ship it'}\n"); err != nil {
		t.Fatal(err)
	}
	list, _ := BotList("bot1")
	if len(list) != 1 || list[0].Name != "deploy" || list[0].Description != "ship it" {
		t.Fatalf("list = %+v", list)
	}
	for _, bad := range []string{"../evil", "a/b", "", "..", "x.js/y"} {
		if err := BotCreate("bot1", bad); err == nil {
			t.Errorf("invalid name %q should be rejected", bad)
		}
	}
	if err := BotDelete("bot1", "deploy"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(botPath(t, "bot1"), "deploy.js")); !os.IsNotExist(err) {
		t.Error("workflow should be gone after BotDelete")
	}
}

func TestPerBotIsolation(t *testing.T) {
	setHome(t)
	if err := BotCreate("alpha", "shared"); err != nil {
		t.Fatal(err)
	}
	if err := BotCreate("beta", "shared"); err != nil {
		t.Fatal(err)
	}
	if err := BotWrite("alpha", "shared", "alpha-only\n"); err != nil {
		t.Fatal(err)
	}
	if got, _ := BotRead("beta", "shared"); got == "alpha-only\n" {
		t.Error("beta's workflow received alpha's edit")
	}
	if err := BotDelete("alpha", "shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(botPath(t, "beta"), "shared.js")); err != nil {
		t.Errorf("beta's workflow must survive alpha's delete: %v", err)
	}
}

func TestInvalidBotID(t *testing.T) {
	setHome(t)
	if _, err := BotList("../bot"); err == nil {
		t.Error("invalid bot id should be rejected")
	}
}
