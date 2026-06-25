package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/lml2468/octobuddy/core/config"
	"github.com/lml2468/octobuddy/core/control"
	"github.com/lml2468/octobuddy/core/cron"
)

// TestCronControlHandlers exercises cron.create/list/delete over the multi-bot
// control handler. After the owner-gate keys off the SERVER-resolved
// owner uid, never the body uid: a forged body uid does not change authorization
// and the created task binds to the resolved owner. The Manager uses a fixed
// clock so the schedule is deterministic.
func TestCronControlHandlers(t *testing.T) {
	const owner = "owner-1"
	env := newCronHandlerTestEnv(t, owner, true)

	assertNoCronBotRejected(t, env.call)
	info := assertForgedCronCreate(t, env.call, env.mgr, owner)
	assertCronListShowsTask(t, env.call, info.ID)
	assertCronDeleteClearsTask(t, env.call, info.ID)
}

type cronHandlerTestEnv struct {
	mgr  *cron.Manager
	call func(string, any) (any, error)
}

func newCronHandlerTestEnv(t *testing.T, owner string, withNoCron bool) cronHandlerTestEnv {
	t.Helper()

	clk := time.Date(2026, 6, 9, 10, 0, 0, 0, time.Local)
	store := cron.NewStore(filepath.Join(t.TempDir(), "cron.json"))
	mgr := cron.NewManager(store, owner, func() time.Time { return clk })

	reg := newBotRegistry(nil)
	b1 := &botRuntime{cfg: config.Resolved{BotID: "b1"}, cron: mgr}
	b1.target = &botTarget{id: "b1", cron: mgr}
	reg.add(b1)
	if withNoCron {
		nocron := &botRuntime{cfg: config.Resolved{BotID: "nocron"}} // cron == nil
		nocron.target = &botTarget{id: "nocron"}
		reg.add(nocron)
	}
	h := makeMultiBotHandler(context.Background(), reg, time.Now())
	call := func(typ string, body any) (any, error) {
		raw, _ := json.Marshal(body)
		return h(typ, raw)
	}
	return cronHandlerTestEnv{mgr: mgr, call: call}
}

func assertNoCronBotRejected(t *testing.T, call func(string, any) (any, error)) {
	t.Helper()

	// Not enabled for the nocron bot.
	if _, err := call("cron.list", control.CronListBody{BotID: "nocron"}); err == nil {
		t.Fatal("cron.list on a bot without cron should error")
	}
}

func assertForgedCronCreate(t *testing.T, call func(string, any) (any, error), mgr *cron.Manager, owner string) control.CronTaskInfo {
	t.Helper()

	// A forged body uid (the deprecated UID field) is NOT an authorization
	// claim: the create still succeeds (gated on the resolved owner). The body
	// FromUID is likewise ignored — a DM task always fires to the OWNER (a
	// scheduled DM may only target the owner, never an arbitrary peer), so the
	// stored FromUID is the owner regardless of what the body carries.
	res, err := call("cron.create", control.CronCreateBody{
		BotID: "b1", UID: "intruder", FromUID: "alice", Schedule: "0 9 * * *", Prompt: "daily standup",
	})
	if err != nil {
		t.Fatalf("create with forged body uid should be gated on server owner, not rejected/forged: %v", err)
	}
	info, ok := res.(control.CronTaskInfo)
	if !ok || info.ID == "" || info.NextRun == "" {
		t.Fatalf("unexpected create result: %#v", res)
	}
	// Verify the stored task: both CreatedBy (auth) and FromUID (fire target)
	// are the resolved owner — the forged body uid "intruder" and the body
	// FromUID "alice" are both dropped.
	stored, err := mgr.List()
	if err != nil || len(stored) != 1 {
		t.Fatalf("list after create: %v %#v", err, stored)
	}
	if stored[0].CreatedBy != owner {
		t.Fatalf("CreatedBy must be the resolved owner, got %q", stored[0].CreatedBy)
	}
	if stored[0].FromUID != owner {
		t.Fatalf("a DM task fires to the owner; FromUID must be %q (body peer %q must be ignored), got %q", owner, "alice", stored[0].FromUID)
	}
	return info
}

func assertCronListShowsTask(t *testing.T, call func(string, any) (any, error), id string) {
	t.Helper()

	// List shows the task.
	listRes, err := call("cron.list", control.CronListBody{BotID: "b1"})
	if err != nil {
		t.Fatalf("cron.list: %v", err)
	}
	tasks := listRes.(control.CronListResponse).Tasks
	if len(tasks) != 1 || tasks[0].ID != id {
		t.Fatalf("list mismatch: %#v", tasks)
	}
}

func assertCronDeleteClearsTask(t *testing.T, call func(string, any) (any, error), id string) {
	t.Helper()

	// Delete is likewise gated on the resolved owner, not the body uid: a forged
	// uid does not block (it's ignored) — the delete succeeds.
	if _, err := call("cron.delete", control.CronDeleteBody{BotID: "b1", UID: "intruder", ID: id}); err != nil {
		t.Fatalf("delete gated on server owner should succeed regardless of body uid: %v", err)
	}
	listRes, _ := call("cron.list", control.CronListBody{BotID: "b1"})
	if n := len(listRes.(control.CronListResponse).Tasks); n != 0 {
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
	b1 := &botRuntime{cfg: config.Resolved{BotID: "b1"}, cron: mgr}
	b1.target = &botTarget{id: "b1", cron: mgr}
	reg.add(b1)
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

// cron.update success path: forged body uid is ignored (owner-gated server-
// side), full update validates schedule + recomputes NextRun, expanded
// CronTaskInfo response carries the new fields the GUI needs (LastRun /
// ChannelID / ChannelType / FromName).
func TestCronUpdateHandler(t *testing.T) {
	const owner = "owner-1"
	env := newCronHandlerTestEnv(t, owner, false)

	id := createCronTaskForUpdate(t, env.call)
	assertCronFullUpdate(t, env.call, id)
	assertCronEnabledOnlyUpdate(t, env.call, id)
}

func createCronTaskForUpdate(t *testing.T, call func(string, any) (any, error)) string {
	t.Helper()

	created, err := call("cron.create", control.CronCreateBody{
		BotID: "b1", Schedule: "0 9 * * *", Prompt: "morning",
		ChannelID: "grp-x", ChannelType: 2, FromName: "stand-up",
	})
	if err != nil {
		t.Fatal(err)
	}
	return created.(control.CronTaskInfo).ID
}

func assertCronFullUpdate(t *testing.T, call func(string, any) (any, error), id string) {
	t.Helper()

	// Full update — change schedule + prompt + target. Body uid forged; ignored.
	res, err := call("cron.update", control.CronUpdateBody{
		BotID: "b1", ID: id, Schedule: "0 18 * * *", Prompt: "evening",
		ChannelID: "grp-y", ChannelType: 2, FromName: "wrap-up",
	})
	if err != nil {
		t.Fatalf("cron.update full: %v", err)
	}
	info := res.(control.CronTaskInfo)
	if info.Prompt != "evening" || info.Schedule != "0 18 * * *" {
		t.Fatalf("update did not apply: %+v", info)
	}
	if info.ChannelID != "grp-y" || info.ChannelType != 2 || info.FromName != "wrap-up" {
		t.Fatalf("expanded CronTaskInfo missing channel coords: %+v", info)
	}
}

func assertCronEnabledOnlyUpdate(t *testing.T, call func(string, any) (any, error), id string) {
	t.Helper()

	// Enabled-only fast path: send only Enabled=false, server preserves all
	// other fields (no echoing of schedule/prompt needed).
	off := false
	res, err := call("cron.update", control.CronUpdateBody{BotID: "b1", ID: id, Enabled: &off})
	if err != nil {
		t.Fatalf("enabled-only update: %v", err)
	}
	info := res.(control.CronTaskInfo)
	if info.Enabled {
		t.Fatal("enabled flag did not flip")
	}
	if info.Prompt != "evening" || info.Schedule != "0 18 * * *" {
		t.Fatalf("enabled-only update wiped other fields: %+v", info)
	}
}

// fireCronTask routes Console-target tasks through gateway.Handle directly,
// bypassing the IM connector — that's how the desktop GUI's CONSOLE_UID
// session receives the synthetic user message + reply round-trip. A
// regression here (e.g. forgetting the new branch in the three-way switch)
// would send Console fires to the connector with an empty ChannelID, which
// would fail or mis-deliver. Routing-decision coverage lives in the e2e
// verify section of the PR plan rather than a unit test here — the
// gateway.Gateway and octo.Connector are concrete types whose stub-out
// would require a wider Sink/Gateway-interface refactor than this change
// warrants. A regression in the routing branch is caught immediately by
// the verify-step "create a Console-target task, observe it land in the
// desktop chat" smoke run.

// TestCronThreadTarget verifies a thread (CommunityTopic = 5) task: the
// compound channel id is stored verbatim, ChannelType persists as 5, and the
// task fires AS the owner (FromUID = owner, like a group — never a peer uid).
func TestCronThreadTarget(t *testing.T) {
	const owner = "owner-1"
	env := newCronHandlerTestEnv(t, owner, false)

	const threadID = "0fff23f5____2069602928229879808"
	res, err := env.call("cron.create", control.CronCreateBody{
		BotID: "b1", Schedule: "0 9 * * *", Prompt: "thread digest",
		ChannelID: threadID, ChannelType: 5,
	})
	if err != nil {
		t.Fatalf("thread-target create: %v", err)
	}
	info := res.(control.CronTaskInfo)
	if info.ChannelID != threadID || info.ChannelType != 5 {
		t.Fatalf("thread coords not preserved: %+v", info)
	}
	stored, err := env.mgr.List()
	if err != nil || len(stored) != 1 {
		t.Fatalf("list after thread create: %v %#v", err, stored)
	}
	if stored[0].ChannelType != cron.ChannelCommunityTopic {
		t.Fatalf("stored ChannelType must be CommunityTopic(5), got %d", stored[0].ChannelType)
	}
	if stored[0].FromUID != owner {
		t.Fatalf("a thread task fires as the owner, FromUID must be %q, got %q", owner, stored[0].FromUID)
	}
}

// TestCronThreadRequiresChannelID verifies a thread/group task without a
// channel id is rejected at create — otherwise it would fire with an
// unroutable empty channel id.
func TestCronThreadRequiresChannelID(t *testing.T) {
	env := newCronHandlerTestEnv(t, "owner-1", false)
	if _, err := env.call("cron.create", control.CronCreateBody{
		BotID: "b1", Schedule: "0 9 * * *", Prompt: "p", ChannelType: 5,
	}); err == nil {
		t.Fatal("thread create without channelId must be refused")
	}
}

// TestCronDMTargetsOwner verifies a DM task fires to the OWNER, never an
// arbitrary peer: a body FromUID is ignored and the stored FromUID is the
// resolved owner. (A scheduled DM to a random peer is a footgun, so the peer
// is never taken from the forgeable body.)
func TestCronDMTargetsOwner(t *testing.T) {
	const owner = "owner-1"
	env := newCronHandlerTestEnv(t, owner, false)

	res, err := env.call("cron.create", control.CronCreateBody{
		BotID: "b1", Schedule: "0 9 * * *", Prompt: "nudge", ChannelType: 1, FromUID: "stranger",
	})
	if err != nil {
		t.Fatalf("DM create should succeed (fires to owner): %v", err)
	}
	info := res.(control.CronTaskInfo)
	if info.ChannelType != 1 {
		t.Fatalf("DM task type must be 1, got %d", info.ChannelType)
	}
	stored, err := env.mgr.List()
	if err != nil || len(stored) != 1 {
		t.Fatalf("list after DM create: %v %#v", err, stored)
	}
	if stored[0].FromUID != owner {
		t.Fatalf("DM fires to the owner; FromUID must be %q, body peer %q must be ignored, got %q", owner, "stranger", stored[0].FromUID)
	}
}
