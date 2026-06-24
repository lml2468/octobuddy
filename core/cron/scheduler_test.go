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
