package cron

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// CreateParams are the inputs to Create.
type CreateParams struct {
	Schedule  string
	Prompt    string
	Recurring *bool // nil = default (cron→true, one-shot→false)
	Coords    SessionCoords
	// RequestUID is the uid asking to create. Must equal the owner uid.
	RequestUID string
}

type cronCreatePlan struct {
	recurring bool
	now       time.Time
	nextRun   int64
}

// Create validates and stores a new task, gated to the bot owner. Returns the
// created task. Mirrors cron_create in cron-tool.ts (owner-gate, schedule
// validation, prompt-byte cap, MAX_TASKS cap re-checked inside the mutator).
func (m *Manager) Create(p CreateParams) (Task, error) {
	if owner := m.owner(); owner == "" || p.RequestUID != owner {
		return Task{}, fmt.Errorf("only the bot owner can create scheduled tasks")
	}
	if p.Coords.ChannelID == "" && p.Coords.FromUID == "" {
		return Task{}, fmt.Errorf("task has no session coords to bind to")
	}
	plan, err := prepareCronCreate(p, m.now())
	if err != nil {
		return Task{}, err
	}
	task := buildCronTask(p, plan)
	capped := false
	if _, err := m.store.Update(func(tasks []Task) ([]Task, bool) {
		// Re-check the cap inside the mutator so a concurrent create can't push us
		// over the limit.
		if len(tasks) >= MaxTasksPerBot {
			capped = true
			return tasks, false
		}
		return append(tasks, task), true
	}); err != nil {
		return Task{}, err
	}
	if capped {
		return Task{}, fmt.Errorf("task limit reached (max %d); delete one first", MaxTasksPerBot)
	}
	return task, nil
}

func prepareCronCreate(p CreateParams, now time.Time) (cronCreatePlan, error) {
	oneShot := isOneShotSchedule(p.Schedule)
	if !ValidateSchedule(p.Schedule) {
		if oneShot {
			return cronCreatePlan{}, fmt.Errorf("one-shot time is invalid: %s", p.Schedule)
		}
		return cronCreatePlan{}, fmt.Errorf("invalid cron expression: %s", p.Schedule)
	}
	if len(p.Prompt) == 0 {
		return cronCreatePlan{}, fmt.Errorf("prompt is required")
	}
	if len(p.Prompt) > MaxPromptBytes {
		return cronCreatePlan{}, fmt.Errorf("prompt too long (max %d bytes)", MaxPromptBytes)
	}
	recurring := !oneShot
	if p.Recurring != nil {
		recurring = *p.Recurring
	}
	next, ok := computeNextRun(p.Schedule, now)
	if !ok {
		if oneShot {
			return cronCreatePlan{}, fmt.Errorf("one-shot time is in the past or invalid")
		}
		return cronCreatePlan{}, fmt.Errorf("schedule never matches (impossible cron): %s", p.Schedule)
	}
	return cronCreatePlan{recurring: recurring, now: now, nextRun: unixMS(next)}, nil
}

func buildCronTask(p CreateParams, plan cronCreatePlan) Task {
	return Task{
		ID:          uuid.NewString(),
		Schedule:    p.Schedule,
		Recurring:   plan.recurring,
		Prompt:      p.Prompt,
		ChannelID:   p.Coords.ChannelID,
		ChannelType: p.Coords.ChannelType,
		FromUID:     p.Coords.FromUID,
		FromName:    p.Coords.FromName,
		CreatedBy:   p.RequestUID,
		Enabled:     true,
		CreatedAt:   unixMS(plan.now),
		LastRun:     0,
		NextRun:     plan.nextRun,
	}
}

// List returns the bot's tasks (no gating — listing is read-only).
func (m *Manager) List() ([]Task, error) {
	return m.store.Load()
}

// Delete removes a task by id, gated to the bot owner. Mirrors cron_delete.
func (m *Manager) Delete(id, requestUID string) error {
	if owner := m.owner(); owner == "" || requestUID != owner {
		return fmt.Errorf("only the bot owner can delete scheduled tasks")
	}
	found := false
	if _, err := m.store.Update(func(tasks []Task) ([]Task, bool) {
		out := tasks[:0:0]
		for _, t := range tasks {
			if t.ID == id {
				found = true
				continue
			}
			out = append(out, t)
		}
		return out, found
	}); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no task with id %s", id)
	}
	return nil
}

// UpdateParams carries a full replacement of a task's mutable fields. ID
// targets the row; CreatedBy/CreatedAt/LastRun are preserved by Update.
// Enabled is a pointer so the GUI's per-row toggle can send enabled-only
// changes by leaving Schedule/Prompt/Coords zero — the mutator detects an
// "enabled-only" body and skips the full validation pass.
type UpdateParams struct {
	ID         string
	Schedule   string
	Prompt     string
	Recurring  *bool
	Coords     SessionCoords
	Enabled    *bool
	RequestUID string
}

type cronUpdatePlan struct {
	enabledOnly    bool
	preserveCoords bool
	recurring      bool
	nextRun        int64
}

// Update mutates an existing task atomically. Same owner-gate model as
// Create/Delete: requester must equal current owner AND the task's CreatedBy
// must also equal that owner (a task left over from a previous owner uid is
// invisible/immutable). On a full update the schedule is re-validated and
// NextRun is recomputed from m.now(); LastRun is preserved so the "last
// fired" indicator survives an edit.
//
// "enabled-only" fast path: when every other field is zero except Enabled,
// the mutator just flips the bit on the matching row — schedule validation
// is skipped (the schedule didn't change). This keeps the per-row GUI
// toggle cheap and prevents the spurious "schedule never matches" failure
// you'd get if you echoed the current schedule back as part of the toggle.
func (m *Manager) Update(p UpdateParams) (Task, error) {
	owner := m.owner()
	if owner == "" || p.RequestUID != owner {
		return Task{}, fmt.Errorf("only the bot owner can update scheduled tasks")
	}
	if p.ID == "" {
		return Task{}, fmt.Errorf("task id is required")
	}
	plan, err := prepareCronUpdate(p, m.now())
	if err != nil {
		return Task{}, err
	}

	var updated Task
	found := false
	if _, err := m.store.Update(func(tasks []Task) ([]Task, bool) {
		out := make([]Task, len(tasks))
		for i, t := range tasks {
			if t.ID == p.ID && t.CreatedBy == owner {
				found = true
				t = applyCronUpdate(t, p, plan)
				updated = t
			}
			out[i] = t
		}
		return out, found
	}); err != nil {
		return Task{}, err
	}
	if !found {
		return Task{}, fmt.Errorf("no task with id %s", p.ID)
	}
	return updated, nil
}

func prepareCronUpdate(p UpdateParams, now time.Time) (cronUpdatePlan, error) {
	plan := cronUpdatePlan{
		enabledOnly:    isEnabledOnlyUpdate(p),
		preserveCoords: shouldPreserveUpdateCoords(p.Coords),
	}
	// Validate full-update fields BEFORE the mutator so a bad request doesn't
	// land in the store.Update call's IO path.
	if plan.enabledOnly {
		return plan, nil
	}
	oneShot := isOneShotSchedule(p.Schedule)
	if !ValidateSchedule(p.Schedule) {
		if oneShot {
			return cronUpdatePlan{}, fmt.Errorf("one-shot time is invalid: %s", p.Schedule)
		}
		return cronUpdatePlan{}, fmt.Errorf("invalid cron expression: %s", p.Schedule)
	}
	if len(p.Prompt) == 0 {
		return cronUpdatePlan{}, fmt.Errorf("prompt is required")
	}
	if len(p.Prompt) > MaxPromptBytes {
		return cronUpdatePlan{}, fmt.Errorf("prompt too long (max %d bytes)", MaxPromptBytes)
	}
	plan.recurring = !oneShot
	if p.Recurring != nil {
		plan.recurring = *p.Recurring
	}
	next, ok := computeNextRun(p.Schedule, now)
	if !ok {
		if oneShot {
			return cronUpdatePlan{}, fmt.Errorf("one-shot time is in the past or invalid")
		}
		return cronUpdatePlan{}, fmt.Errorf("schedule never matches (impossible cron): %s", p.Schedule)
	}
	plan.nextRun = unixMS(next)
	return plan, nil
}

func isEnabledOnlyUpdate(p UpdateParams) bool {
	return p.Schedule == "" && p.Prompt == "" && p.Recurring == nil && p.Enabled != nil &&
		p.Coords.ChannelID == "" && p.Coords.FromUID == "" && p.Coords.ChannelType == 0 && p.Coords.FromName == ""
}

func shouldPreserveUpdateCoords(coords SessionCoords) bool {
	// A full update with empty coords means "leave the existing target binding
	// alone" — the GUI's edit modal sends blank channel/from fields for "I'm
	// only editing schedule/prompt" intent. Without this the handler would
	// silently rebind every DM task to the owner's self-DM on any unrelated
	// edit. Detected as: ChannelID AND FromUID both empty AND ChannelType zero
	// (= no explicit target shape in the body).
	return coords.ChannelID == "" && coords.FromUID == "" && coords.ChannelType == 0
}

func applyCronUpdate(t Task, p UpdateParams, plan cronUpdatePlan) Task {
	if plan.enabledOnly {
		t.Enabled = *p.Enabled
		return t
	}
	t.Schedule = p.Schedule
	t.Recurring = plan.recurring
	t.Prompt = p.Prompt
	t.NextRun = plan.nextRun
	if !plan.preserveCoords {
		// Caller wants to rebind the target. Replace coords.
		t.ChannelID = p.Coords.ChannelID
		t.ChannelType = p.Coords.ChannelType
		t.FromUID = p.Coords.FromUID
	}
	// FromName is treated as a partial-edit field on its own: empty = preserve
	// (matches the "I'm not changing the display name" GUI default), non-empty
	// = rewrite. Without this an edit that blanks FromName would erase the bot's
	// display name in every future fire.
	if p.Coords.FromName != "" {
		t.FromName = p.Coords.FromName
	}
	if p.Enabled != nil {
		t.Enabled = *p.Enabled
	}
	return t
}
