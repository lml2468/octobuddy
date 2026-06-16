package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/lml2468/xclaw/core/config"
	"github.com/lml2468/xclaw/core/control"
	"github.com/lml2468/xclaw/core/cron"
)

// TestCronControlHandlers exercises cron.create/list/delete over the multi-bot
// control handler. After MLT-29 the owner-gate keys off the SERVER-resolved
// owner uid, never the body uid: a forged body uid does not change authorization
// and the created task binds to the resolved owner. The Manager uses a fixed
// clock so the schedule is deterministic.
func TestCronControlHandlers(t *testing.T) {
	const owner = "owner-1"
	clk := time.Date(2026, 6, 9, 10, 0, 0, 0, time.Local)
	store := cron.NewStore(filepath.Join(t.TempDir(), "cron.json"))
	mgr := cron.NewManager(store, owner, func() time.Time { return clk })

	reg := newBotRegistry(nil)
	reg.add(&botRuntime{cfg: config.Resolved{BotID: "b1"}, cron: mgr})
	reg.add(&botRuntime{cfg: config.Resolved{BotID: "nocron"}}) // cron == nil
	h := makeMultiBotHandler(context.Background(), reg, time.Now())

	call := func(typ string, body any) (any, error) {
		raw, _ := json.Marshal(body)
		return h(typ, raw)
	}

	// Not enabled for the nocron bot.
	if _, err := call("cron.list", control.CronListBody{BotID: "nocron"}); err == nil {
		t.Fatal("cron.list on a bot without cron should error")
	}

	// A forged body uid is NOT an authorization claim: the create still succeeds
	// (gated on the resolved owner) AND binds to the owner, not the forged uid.
	res, err := call("cron.create", control.CronCreateBody{
		BotID: "b1", UID: "intruder", Schedule: "0 9 * * *", Prompt: "daily standup",
	})
	if err != nil {
		t.Fatalf("create with forged body uid should be gated on server owner, not rejected/forged: %v", err)
	}
	info, ok := res.(control.CronTaskInfo)
	if !ok || info.ID == "" || info.NextRun == "" {
		t.Fatalf("unexpected create result: %#v", res)
	}
	// Verify the stored task is bound to the owner, never the forged body uid.
	stored, err := mgr.List()
	if err != nil || len(stored) != 1 {
		t.Fatalf("list after create: %v %#v", err, stored)
	}
	if stored[0].FromUID != owner || stored[0].CreatedBy != owner {
		t.Fatalf("task must bind to resolved owner, got FromUID=%q CreatedBy=%q",
			stored[0].FromUID, stored[0].CreatedBy)
	}

	// List shows the task.
	listRes, err := call("cron.list", control.CronListBody{BotID: "b1"})
	if err != nil {
		t.Fatalf("cron.list: %v", err)
	}
	tasks := listRes.([]control.CronTaskInfo)
	if len(tasks) != 1 || tasks[0].ID != info.ID {
		t.Fatalf("list mismatch: %#v", tasks)
	}

	// Delete is likewise gated on the resolved owner, not the body uid: a forged
	// uid does not block (it's ignored) — the delete succeeds.
	if _, err := call("cron.delete", control.CronDeleteBody{BotID: "b1", UID: "intruder", ID: info.ID}); err != nil {
		t.Fatalf("delete gated on server owner should succeed regardless of body uid: %v", err)
	}
	listRes, _ = call("cron.list", control.CronListBody{BotID: "b1"})
	if n := len(listRes.([]control.CronTaskInfo)); n != 0 {
		t.Fatalf("expected empty list after delete, got %d", n)
	}
}

// TestCronControlHandlersNoOwner verifies that when the bot has no resolved owner
// yet (pre-registration), create/delete are refused — there is no verified
// identity to gate on, so privileged cron ops must fail closed.
func TestCronControlHandlersNoOwner(t *testing.T) {
	store := cron.NewStore(filepath.Join(t.TempDir(), "cron.json"))
	mgr := cron.NewManager(store, "", nil) // empty owner = unresolved

	reg := newBotRegistry(nil)
	reg.add(&botRuntime{cfg: config.Resolved{BotID: "b1"}, cron: mgr})
	h := makeMultiBotHandler(context.Background(), reg, time.Now())

	raw, _ := json.Marshal(control.CronCreateBody{BotID: "b1", Schedule: "0 9 * * *", Prompt: "p"})
	if _, err := h("cron.create", raw); err == nil {
		t.Fatal("cron.create with no resolved owner must be refused")
	}
	rawDel, _ := json.Marshal(control.CronDeleteBody{BotID: "b1", ID: "x"})
	if _, err := h("cron.delete", rawDel); err == nil {
		t.Fatal("cron.delete with no resolved owner must be refused")
	}
}
