package cron

import (
	"sync"
	"testing"
	"time"
)

func TestSchedulerFiresDueRecurringAndAdvances(t *testing.T) {
	// now = 10:00:30; a "*/1 * * * *" task next fires at 10:01.
	clk := &frozenClock{t: time.Date(2026, 6, 9, 10, 0, 30, 0, time.Local)}
	m := newManager(t, "o", clk)
	fireCount, fireAt := captureFires(m)

	task := createRecurringTask(t, m)

	// Not yet due (next at 10:01, now 10:00:30).
	m.Tick()
	if n := fireCount(); n != 0 {
		t.Fatalf("should not fire before due, got %d", n)
	}

	// Advance past the due minute.
	clk.advance(time.Minute)
	m.Tick()
	if n := fireCount(); n != 1 {
		t.Fatalf("expected 1 fire, got %d", n)
	}
	assertRecurringFire(t, fireAt(0), task, m)
}

func captureFires(m *Manager) (func() int, func(int) Fire) {
	var mu sync.Mutex
	var fired []Fire
	m.OnFire(func(f Fire) {
		mu.Lock()
		fired = append(fired, f)
		mu.Unlock()
	})
	return func() int {
			mu.Lock()
			defer mu.Unlock()
			return len(fired)
		}, func(i int) Fire {
			mu.Lock()
			defer mu.Unlock()
			return fired[i]
		}
}

func createRecurringTask(t *testing.T, m *Manager) Task {
	t.Helper()

	task, err := m.Create(CreateParams{
		Schedule: "* * * * *", Prompt: "tick", Coords: SessionCoords{ChannelID: "c1", ChannelType: 2, FromUID: "o"}, RequestUID: "o",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return task
}

func assertRecurringFire(t *testing.T, fire Fire, task Task, m *Manager) {
	t.Helper()

	if fire.Task.Prompt != "tick" {
		t.Errorf("fired wrong task: %+v", fire.Task)
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
