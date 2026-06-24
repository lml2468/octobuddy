package cron

import (
	"testing"
	"time"
)

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
