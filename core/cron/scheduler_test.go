package cron

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	return NewStore(filepath.Join(dir, "cron.json"))
}

func TestStoreLoadEmptyWhenAbsent(t *testing.T) {
	s := tempStore(t)
	tasks, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected empty, got %d", len(tasks))
	}
}

func TestStorePersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cron.json")

	s1 := NewStore(path)
	if _, err := s1.Update(func(tasks []Task) ([]Task, bool) {
		return append(tasks, Task{ID: "a", Schedule: "* * * * *", Prompt: "hi", Enabled: true}), true
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Mode 0600 on the written file.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("cron.json mode = %o, want 0600", info.Mode().Perm())
	}

	s2 := NewStore(path)
	tasks, err := s2.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "a" {
		t.Fatalf("expected persisted task a, got %+v", tasks)
	}
}

func TestStoreUpdateSkipsWriteWhenUnchanged(t *testing.T) {
	s := tempStore(t)
	// changed=false → no file written.
	if _, err := s.Update(func(tasks []Task) ([]Task, bool) { return tasks, false }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := os.Stat(s.path); !os.IsNotExist(err) {
		t.Errorf("file should not exist after a no-op update, err=%v", err)
	}
}

// frozenClock returns a fixed time, advanceable by tests.
type frozenClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *frozenClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *frozenClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newManager(t *testing.T, owner string, clk *frozenClock) *Manager {
	t.Helper()
	m := NewManager(tempStore(t), owner, clk.now)
	// Production fires onFire on its own goroutine so a long turn doesn't
	// block subsequent Ticks; tests need sync dispatch so `Tick;
	// checkFireCount` works without a poll-wait.
	m.fireSync = true
	return m
}

func TestCreateOwnerGate(t *testing.T) {
	clk := &frozenClock{t: time.Date(2026, 6, 9, 10, 0, 0, 0, time.Local)}
	m := newManager(t, "owner-1", clk)

	// Non-owner is rejected.
	_, err := m.Create(CreateParams{
		Schedule: "* * * * *", Prompt: "p", Coords: SessionCoords{FromUID: "intruder"}, RequestUID: "intruder",
	})
	if err == nil {
		t.Fatal("non-owner create must be rejected")
	}

	// Owner succeeds.
	task, err := m.Create(CreateParams{
		Schedule: "* * * * *", Prompt: "p", Coords: SessionCoords{FromUID: "owner-1"}, RequestUID: "owner-1",
	})
	if err != nil {
		t.Fatalf("owner create: %v", err)
	}
	if task.ID == "" || task.NextRun == 0 {
		t.Errorf("created task missing id/nextRun: %+v", task)
	}
}

func TestCreateEmptyOwnerRejects(t *testing.T) {
	clk := &frozenClock{t: time.Now()}
	m := newManager(t, "", clk) // no owner resolved yet
	_, err := m.Create(CreateParams{
		Schedule: "* * * * *", Prompt: "p", Coords: SessionCoords{FromUID: "x"}, RequestUID: "x",
	})
	if err == nil {
		t.Fatal("create with empty owner must be rejected")
	}
}

func TestCreateValidatesScheduleAndPrompt(t *testing.T) {
	clk := &frozenClock{t: time.Date(2026, 6, 9, 10, 0, 0, 0, time.Local)}
	m := newManager(t, "o", clk)
	base := func() CreateParams {
		return CreateParams{Schedule: "* * * * *", Prompt: "p", Coords: SessionCoords{FromUID: "o"}, RequestUID: "o"}
	}

	bad := base()
	bad.Schedule = "nonsense"
	if _, err := m.Create(bad); err == nil {
		t.Error("invalid schedule must be rejected")
	}

	past := base()
	past.Schedule = "2000-01-01T00:00:00Z"
	if _, err := m.Create(past); err == nil {
		t.Error("past one-shot must be rejected")
	}

	long := base()
	long.Prompt = string(make([]byte, MaxPromptBytes+1))
	if _, err := m.Create(long); err == nil {
		t.Error("over-long prompt must be rejected")
	}

	empty := base()
	empty.Prompt = ""
	if _, err := m.Create(empty); err == nil {
		t.Error("empty prompt must be rejected")
	}
}

func TestCreateCapEnforced(t *testing.T) {
	clk := &frozenClock{t: time.Date(2026, 6, 9, 10, 0, 0, 0, time.Local)}
	m := newManager(t, "o", clk)
	for i := 0; i < MaxTasksPerBot; i++ {
		if _, err := m.Create(CreateParams{
			Schedule: "* * * * *", Prompt: "p", Coords: SessionCoords{FromUID: "o"}, RequestUID: "o",
		}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := m.Create(CreateParams{
		Schedule: "* * * * *", Prompt: "p", Coords: SessionCoords{FromUID: "o"}, RequestUID: "o",
	}); err == nil {
		t.Fatalf("create past cap (%d) must be rejected", MaxTasksPerBot)
	}
}

func TestDeleteOwnerGateAndMissing(t *testing.T) {
	clk := &frozenClock{t: time.Date(2026, 6, 9, 10, 0, 0, 0, time.Local)}
	m := newManager(t, "o", clk)
	task, err := m.Create(CreateParams{
		Schedule: "* * * * *", Prompt: "p", Coords: SessionCoords{FromUID: "o"}, RequestUID: "o",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := m.Delete(task.ID, "intruder"); err == nil {
		t.Error("non-owner delete must be rejected")
	}
	if err := m.Delete("no-such-id", "o"); err == nil {
		t.Error("deleting a missing id must error")
	}
	if err := m.Delete(task.ID, "o"); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	tasks, _ := m.List()
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks after delete, got %d", len(tasks))
	}
}

func TestSchedulerFiresDueRecurringAndAdvances(t *testing.T) {
	// now = 10:00:30; a "*/1 * * * *" task next fires at 10:01.
	clk := &frozenClock{t: time.Date(2026, 6, 9, 10, 0, 30, 0, time.Local)}
	m := newManager(t, "o", clk)

	var mu sync.Mutex
	var fired []Fire
	m.OnFire(func(f Fire) {
		mu.Lock()
		fired = append(fired, f)
		mu.Unlock()
	})

	task, err := m.Create(CreateParams{
		Schedule: "* * * * *", Prompt: "tick", Coords: SessionCoords{ChannelID: "c1", ChannelType: 2, FromUID: "o"}, RequestUID: "o",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Not yet due (next at 10:01, now 10:00:30).
	m.Tick()
	mu.Lock()
	n := len(fired)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("should not fire before due, got %d", n)
	}

	// Advance past the due minute.
	clk.advance(time.Minute)
	m.Tick()
	mu.Lock()
	n = len(fired)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("expected 1 fire, got %d", n)
	}
	if fired[0].Task.Prompt != "tick" {
		t.Errorf("fired wrong task: %+v", fired[0].Task)
	}

	// Recurring task survives with an advanced nextRun.
	tasks, _ := m.List()
	if len(tasks) != 1 {
		t.Fatalf("recurring task should survive, got %d", len(tasks))
	}
	if tasks[0].NextRun <= task.NextRun {
		t.Errorf("nextRun should advance: was %d now %d", task.NextRun, tasks[0].NextRun)
	}
	if tasks[0].LastRun == 0 {
		t.Error("lastRun should be set after a fire")
	}
}

func TestSchedulerOneShotFiresOnceThenDropped(t *testing.T) {
	clk := &frozenClock{t: time.Date(2026, 6, 9, 10, 0, 0, 0, time.Local)}
	m := newManager(t, "o", clk)

	var count int
	var mu sync.Mutex
	m.OnFire(func(Fire) { mu.Lock(); count++; mu.Unlock() })

	// One-shot one minute out, bound to a DM.
	when := clk.now().Add(time.Minute).UTC().Format(time.RFC3339)
	if _, err := m.Create(CreateParams{
		Schedule: when, Prompt: "once", Coords: SessionCoords{FromUID: "o"}, RequestUID: "o",
	}); err != nil {
		t.Fatalf("create one-shot: %v", err)
	}

	clk.advance(2 * time.Minute)
	m.Tick()
	m.Tick() // second tick must not refire

	mu.Lock()
	c := count
	mu.Unlock()
	if c != 1 {
		t.Fatalf("one-shot should fire exactly once, got %d", c)
	}
	tasks, _ := m.List()
	if len(tasks) != 0 {
		t.Errorf("one-shot should be dropped after firing, got %d", len(tasks))
	}
}

func TestSchedulerSkipsDisabled(t *testing.T) {
	clk := &frozenClock{t: time.Date(2026, 6, 9, 10, 0, 0, 0, time.Local)}
	m := newManager(t, "o", clk)
	fired := 0
	m.OnFire(func(Fire) { fired++ })

	// Inject a disabled task directly into the store.
	if _, err := m.store.Update(func(tasks []Task) ([]Task, bool) {
		return append(tasks, Task{
			ID: "d", Schedule: "* * * * *", Prompt: "x", Enabled: false,
			NextRun: unixMS(clk.now().Add(-time.Minute)),
		}), true
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m.Tick()
	if fired != 0 {
		t.Errorf("disabled task should not fire, fired=%d", fired)
	}
}

// TestOwnerUIDConcurrentAccess exercises SetOwnerUID concurrently with Create so
// `go test -race` proves the ownerUID field is properly synchronized.
func TestOwnerUIDConcurrentAccess(t *testing.T) {
	clk := &frozenClock{t: time.Date(2026, 6, 9, 10, 0, 0, 0, time.Local)}
	m := newManager(t, "owner-1", clk)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			m.SetOwnerUID("owner-1")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			// Create reads ownerUID under the gate; errors (cap, etc.) are fine —
			// the point is the concurrent read/write must be race-free.
			_, _ = m.Create(CreateParams{
				Schedule: "* * * * *", Prompt: "p",
				Coords: SessionCoords{FromUID: "owner-1"}, RequestUID: "owner-1",
			})
		}
	}()
	wg.Wait()
}

// TestTickDoesNotBlockOnSlowFire is the regression for the F3 bug:
// Tick used to call onFire inline, so a long-running turn for one task would
// starve subsequent ticks (the timer's channel only buffers one tick). Now
// onFire runs on its own goroutine. With fireSync=false (production
// behavior), a fire callback that blocks for 200 ms must NOT prevent Tick
// from returning promptly so the next tick window stays open.
func TestTickDoesNotBlockOnSlowFire(t *testing.T) {
	clk := &frozenClock{t: time.Date(2026, 6, 9, 10, 0, 30, 0, time.Local)}
	m := newManager(t, "o", clk)
	m.fireSync = false // exercise the production dispatch path

	fireStarted := make(chan struct{}, 1)
	fireRelease := make(chan struct{})
	m.OnFire(func(f Fire) {
		fireStarted <- struct{}{}
		<-fireRelease // hold the goroutine open
	})
	if _, err := m.Create(CreateParams{
		Schedule: "* * * * *", Prompt: "slow", Coords: SessionCoords{ChannelID: "c1", ChannelType: 2, FromUID: "o"}, RequestUID: "o",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	clk.advance(time.Minute) // task now due

	// Tick must return within a generous budget even though the fire goroutine
	// is still blocked. Generous = 500 ms; production timer cadence is 1 min,
	// so this is a 120× safety margin.
	done := make(chan struct{})
	go func() {
		m.Tick()
		close(done)
	}()
	select {
	case <-done:
		// good — Tick returned without waiting for the fire goroutine
	case <-time.After(500 * time.Millisecond):
		close(fireRelease)
		t.Fatal("Tick blocked on slow fire — should have dispatched and returned immediately")
	}
	// Confirm the fire goroutine actually started (catches a regression where
	// the dispatch was accidentally lost).
	select {
	case <-fireStarted:
	case <-time.After(500 * time.Millisecond):
		close(fireRelease)
		t.Fatal("dispatched fire goroutine never ran")
	}
	close(fireRelease)
}

// Update is owner-gated identically to Create + Delete: only the
// server-resolved owner uid can mutate, and a task whose CreatedBy doesn't
// match the current owner is invisible (left over from a prior token rotation).
func TestUpdateOwnerGate(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	m := newManager(t, "owner-1", clk)
	created, err := m.Create(CreateParams{
		Schedule: "* * * * *", Prompt: "p", Coords: SessionCoords{FromUID: "owner-1"}, RequestUID: "owner-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Non-owner request rejected.
	if _, err := m.Update(UpdateParams{
		ID: created.ID, Schedule: "0 9 * * *", Prompt: "q",
		Coords: SessionCoords{FromUID: "owner-1"}, RequestUID: "attacker",
	}); err == nil {
		t.Fatal("non-owner update must be rejected")
	}
	// Owner request succeeds.
	updated, err := m.Update(UpdateParams{
		ID: created.ID, Schedule: "0 9 * * *", Prompt: "q",
		Coords: SessionCoords{FromUID: "owner-1"}, RequestUID: "owner-1",
	})
	if err != nil {
		t.Fatalf("owner update: %v", err)
	}
	if updated.Prompt != "q" || updated.Schedule != "0 9 * * *" {
		t.Fatalf("update did not apply: %+v", updated)
	}
}

// Changing the schedule must recompute NextRun from the manager's clock so the
// next tick fires at the new cadence — otherwise the old NextRun would survive
// the edit and the user would be confused why the "every minute" task they
// just changed to "every hour" fired one more time.
func TestUpdateScheduleRecomputesNextRun(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	m := newManager(t, "owner-1", clk)
	created, err := m.Create(CreateParams{
		Schedule: "* * * * *", Prompt: "p", Coords: SessionCoords{FromUID: "owner-1"}, RequestUID: "owner-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	firstNext := created.NextRun
	// Advance the clock 10 minutes — without recompute the new task would
	// still carry the old (now-stale) NextRun.
	clk.t = clk.t.Add(10 * time.Minute)
	updated, err := m.Update(UpdateParams{
		ID: created.ID, Schedule: "0 0 * * *", Prompt: "p",
		Coords: SessionCoords{FromUID: "owner-1"}, RequestUID: "owner-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.NextRun == firstNext {
		t.Fatal("schedule change must recompute NextRun")
	}
	// LastRun preserved across edits (currently zero — the test asserts the
	// invariant; a regression would surface as LastRun reset to 0 here too,
	// which is benign for this case but spec-relevant).
	if updated.LastRun != created.LastRun {
		t.Fatalf("LastRun must survive an update: was %d, now %d", created.LastRun, updated.LastRun)
	}
}

// A task created under owner A then rotated to owner B is invisible to B —
// Update returns "no task with id X" rather than mutating someone else's data.
func TestUpdateForeignCreatedByRejected(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	m := newManager(t, "owner-A", clk)
	created, err := m.Create(CreateParams{
		Schedule: "* * * * *", Prompt: "p", Coords: SessionCoords{FromUID: "owner-A"}, RequestUID: "owner-A",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Rotate to a new owner. SetOwnerUID prunes foreign tasks today, so we
	// must reload through a fresh manager pointing at the same store to
	// reproduce the "task exists on disk under a different CreatedBy" state.
	m2 := NewManager(m.store, "owner-B", clk.now)
	if _, err := m2.Update(UpdateParams{
		ID: created.ID, Schedule: "0 9 * * *", Prompt: "x",
		Coords: SessionCoords{FromUID: "owner-B"}, RequestUID: "owner-B",
	}); err == nil {
		t.Fatal("update of foreign-CreatedBy task must be rejected")
	}
}

// Updating an unknown id surfaces a clear error rather than silently
// no-op'ing — the GUI surfaces this in the modal so the user sees "your edit
// targets a task that no longer exists" instead of a confusing success.
func TestUpdateUnknownIDRejected(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	m := newManager(t, "owner-1", clk)
	if _, err := m.Update(UpdateParams{
		ID: "no-such-id", Schedule: "* * * * *", Prompt: "p",
		Coords: SessionCoords{FromUID: "owner-1"}, RequestUID: "owner-1",
	}); err == nil {
		t.Fatal("update of unknown id must be rejected")
	}
}

// The enabled-only fast path lets the GUI's per-row toggle flip Enabled
// without echoing the full task back — and must NOT trigger the "task has no
// session coords" validation, since the body is intentionally empty there.
func TestUpdateEnabledOnlyFastPath(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	m := newManager(t, "owner-1", clk)
	created, err := m.Create(CreateParams{
		Schedule: "* * * * *", Prompt: "p", Coords: SessionCoords{FromUID: "owner-1"}, RequestUID: "owner-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created.Enabled {
		t.Fatal("freshly-created task should be Enabled by default")
	}
	off := false
	updated, err := m.Update(UpdateParams{
		ID: created.ID, Enabled: &off, RequestUID: "owner-1",
	})
	if err != nil {
		t.Fatalf("enabled-only update: %v", err)
	}
	if updated.Enabled {
		t.Fatal("Enabled flag did not flip")
	}
	if updated.Schedule != created.Schedule || updated.Prompt != created.Prompt {
		t.Fatal("enabled-only update must NOT clear other fields")
	}
}

// Update's "preserve coords" semantics: when the body's coord triplet is
// all-zero (no ChannelID, no FromUID, ChannelType == 0), the mutator must
// leave the stored target alone. Without this, the GUI's "I'm only editing
// schedule" intent (which sends blank channel/from fields) would silently
// rebind every DM task to whatever fallback the handler stamped — the
// showstopper that motivated the FromUID refactor.
func TestUpdatePreservesCoordsWhenZero(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	m := newManager(t, "owner-1", clk)
	created, err := m.Create(CreateParams{
		Schedule: "* * * * *", Prompt: "p",
		Coords:     SessionCoords{FromUID: "alice", FromName: "Alice"},
		RequestUID: "owner-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := m.Update(UpdateParams{
		ID: created.ID, Schedule: "0 9 * * *", Prompt: "q",
		Coords: SessionCoords{}, RequestUID: "owner-1",
	})
	if err != nil {
		t.Fatalf("update with zero coords: %v", err)
	}
	if updated.FromUID != "alice" {
		t.Fatalf("zero-coords update must preserve FromUID, got %q", updated.FromUID)
	}
	if updated.FromName != "Alice" {
		t.Fatalf("zero-coords update must preserve FromName, got %q", updated.FromName)
	}
	if updated.Schedule != "0 9 * * *" || updated.Prompt != "q" {
		t.Fatalf("schedule/prompt edit did not apply: %+v", updated)
	}
}

// Non-zero coords in the body DO rebind — the explicit "change target"
// path the user takes when they actually fill the form fields.
func TestUpdateRebindsCoordsWhenNonZero(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	m := newManager(t, "owner-1", clk)
	created, err := m.Create(CreateParams{
		Schedule: "* * * * *", Prompt: "p",
		Coords:     SessionCoords{FromUID: "alice"},
		RequestUID: "owner-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := m.Update(UpdateParams{
		ID: created.ID, Schedule: "* * * * *", Prompt: "p",
		Coords:     SessionCoords{FromUID: "bob", ChannelType: 1},
		RequestUID: "owner-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.FromUID != "bob" {
		t.Fatalf("non-zero coords update must rebind FromUID, got %q", updated.FromUID)
	}
}
