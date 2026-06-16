// Cron task management + the resident scheduler (port of cron-scheduler.ts and
// cron-tool.ts). The cc-channel cron tool surfaced as an in-process MCP server
// the agent called; in xclaw the same create/list/delete operations are exposed
// over the control bus instead (see core/cmd/xclawd/control.go). The owner-gate
// and caps are preserved here as the server-side source of truth — not LLM
// judgment — so a prompt-injected agent or untrusted IM user cannot register a
// malicious recurring task.
package cron

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// CronTickInterval is how often the scheduler scans cron.json. 30s → ≤30s firing
// latency (port of CRON_TICK_MS).
const CronTickInterval = 30 * time.Second

// SessionCoords are the raw coords of the session creating a task — what a fired
// task binds to (fires + replies there). Mirrors CronSessionCoords.
type SessionCoords struct {
	ChannelID   string
	ChannelType ChannelKind
	FromUID     string
	FromName    string
}

// Fire is the synthetic message a due task produces. The scheduler hands it to
// the OnFire callback, which the gateway wires to the same path real inbound
// messages use (with CronFire=true to bypass the mention + rate gates).
type Fire struct {
	Task Task
}

// Manager owns a bot's cron store and exposes the owner-gated create/list/delete
// operations plus the scheduler loop. The owner uid gates create/delete; the
// clock is injected so tests are deterministic.
type Manager struct {
	store *Store
	now   func() time.Time
	label string // log prefix, e.g. "[bot-id] "

	// ownerMu guards ownerUID: SetOwnerUID is called from the connector's Run
	// goroutine after each (re)registration, while Create/Delete read it from the
	// control-bus handler goroutine. The lock makes that cross-goroutine access
	// data-race-free.
	ownerMu  sync.RWMutex
	ownerUID string

	timer  *time.Ticker
	stopCh chan struct{}
	onFire func(Fire)
}

// NewManager builds a cron Manager. ownerUID gates create/delete (empty disables
// creation entirely). now defaults to time.Now when nil.
func NewManager(store *Store, ownerUID string, now func() time.Time) *Manager {
	if now == nil {
		now = time.Now
	}
	return &Manager{store: store, ownerUID: ownerUID, now: now}
}

// SetLabel sets a log prefix (multi-bot mode).
func (m *Manager) SetLabel(label string) { m.label = label }

// SetOwnerUID updates the owner uid (resolved after bot registration). Safe to
// call from any goroutine — guarded by ownerMu. The scheduler loop does not read
// it (only Create/Delete do).
func (m *Manager) SetOwnerUID(uid string) {
	m.ownerMu.Lock()
	m.ownerUID = uid
	m.ownerMu.Unlock()
}

// owner returns the current owner uid under the read lock.
func (m *Manager) owner() string {
	m.ownerMu.RLock()
	defer m.ownerMu.RUnlock()
	return m.ownerUID
}

// OwnerUID returns the resolved owner uid ("" if not yet registered). The
// control-bus handler uses this as the *verified* requester identity for
// create/delete instead of a client-supplied uid, which is forgeable (the agent
// reaches cron over the bus via an agent-controlled CLI, so a prompt injection
// could otherwise assert the owner's uid). See core/cmd/xclawd/control.go.
func (m *Manager) OwnerUID() string { return m.owner() }

// CreateParams are the inputs to Create.
type CreateParams struct {
	Schedule  string
	Prompt    string
	Recurring *bool // nil = default (cron→true, one-shot→false)
	Coords    SessionCoords
	// RequestUID is the uid asking to create. Must equal the owner uid.
	RequestUID string
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
	oneShot := isOneShotSchedule(p.Schedule)
	if !ValidateSchedule(p.Schedule) {
		if oneShot {
			return Task{}, fmt.Errorf("one-shot time is invalid: %s", p.Schedule)
		}
		return Task{}, fmt.Errorf("invalid cron expression: %s", p.Schedule)
	}
	if len(p.Prompt) == 0 {
		return Task{}, fmt.Errorf("prompt is required")
	}
	if len(p.Prompt) > MaxPromptBytes {
		return Task{}, fmt.Errorf("prompt too long (max %d bytes)", MaxPromptBytes)
	}
	recurring := !oneShot
	if p.Recurring != nil {
		recurring = *p.Recurring
	}
	now := m.now()
	next, ok := computeNextRun(p.Schedule, recurring, now)
	if !ok {
		if oneShot {
			return Task{}, fmt.Errorf("one-shot time is in the past or invalid")
		}
		return Task{}, fmt.Errorf("schedule never matches (impossible cron): %s", p.Schedule)
	}
	task := Task{
		ID:          uuid.NewString(),
		Schedule:    p.Schedule,
		Recurring:   recurring,
		Prompt:      p.Prompt,
		ChannelID:   p.Coords.ChannelID,
		ChannelType: p.Coords.ChannelType,
		FromUID:     p.Coords.FromUID,
		FromName:    p.Coords.FromName,
		CreatedBy:   p.RequestUID,
		Enabled:     true,
		CreatedAt:   unixMS(now),
		LastRun:     0,
		NextRun:     unixMS(next),
	}
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

// OnFire sets the callback invoked when a task is due (= the gateway's inbound
// path). Must be set before Start.
func (m *Manager) OnFire(fn func(Fire)) { m.onFire = fn }

// Start arms the periodic scan. Idempotent; runs until Stop or until the loop's
// stop channel is closed.
func (m *Manager) Start() {
	if m.timer != nil {
		return
	}
	m.timer = time.NewTicker(CronTickInterval)
	m.stopCh = make(chan struct{})
	go func() {
		for {
			select {
			case <-m.stopCh:
				return
			case <-m.timer.C:
				m.Tick()
			}
		}
	}()
}

// Stop halts the scan.
func (m *Manager) Stop() {
	if m.timer == nil {
		return
	}
	m.timer.Stop()
	close(m.stopCh)
	m.timer = nil
}

// Tick performs one scan: fire due tasks, advance/drop them, persist. Exposed
// for tests. Never panics out — a failing onFire is logged and skipped, never
// crashing the loop. Mirrors cron-scheduler.ts tick(): a single atomic
// read-modify-write fires due tasks and persists the survivor set in one pass,
// so a concurrent create/delete can't lose updates.
func (m *Manager) Tick() {
	now := m.now()
	nowMS := unixMS(now)
	var fires []Fire
	_, err := m.store.Update(func(tasks []Task) ([]Task, bool) {
		survivors := make([]Task, 0, len(tasks))
		changed := false
		for _, task := range tasks {
			if !task.Enabled || task.NextRun == 0 || task.NextRun > nowMS {
				survivors = append(survivors, task)
				continue
			}
			changed = true
			// Defer the actual fire until after the store write returns, so onFire
			// (which drives a full turn) does not run while we hold the store mutex.
			fires = append(fires, Fire{Task: task})
			task.LastRun = nowMS
			if task.Recurring {
				if next, ok := computeNextRun(task.Schedule, true, now); ok {
					task.NextRun = unixMS(next)
				} else {
					task.NextRun = 0 // no future occurrence → inert
				}
				survivors = append(survivors, task)
			}
			// one-shot: drop (not appended to survivors)
		}
		return survivors, changed
	})
	if err != nil {
		fmt.Printf("%scron: tick failed: %v\n", m.label, err)
		return
	}
	for _, f := range fires {
		late := time.Duration(nowMS-f.Task.NextRun) * time.Millisecond
		if late >= time.Minute {
			fmt.Printf("%scron: task %s (%s) fired %d min late (catch-up)\n",
				m.label, f.Task.ID, f.Task.Schedule, int(late.Minutes()))
		}
		if m.onFire != nil {
			m.safeFire(f)
		}
	}
}

// safeFire invokes onFire, recovering from a panic so one bad fire never crashes
// the scheduler loop (cc-channel's best-effort guarantee).
func (m *Manager) safeFire(f Fire) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("%scron: onFire panicked for %s: %v\n", m.label, f.Task.ID, r)
		}
	}()
	m.onFire(f)
}

// unixMS converts a time to Unix milliseconds (the on-disk unit, matching
// cc-channel's Date.now()).
func unixMS(t time.Time) int64 { return t.UnixNano() / int64(time.Millisecond) }
