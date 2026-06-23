// Cron task management + the resident scheduler (port of cron-scheduler.ts and
// cron-tool.ts). The cc-channel cron tool surfaced as an in-process MCP server
// the agent called; in octobuddy the same create/list/delete operations are exposed
// over the control bus instead (see core/cmd/octobuddy-daemon/control.go). The owner-gate
// and caps are preserved here as the server-side source of truth — not LLM
// judgment — so a prompt-injected agent or untrusted IM user cannot register a
// malicious recurring task.
package cron

import (
	"fmt"
	"os"
	"sync"
	"time"
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

	// runMu guards the timer/stopCh pair against concurrent Start/Stop calls
	// (e.g. control-bus handlers running on different goroutines). Without it
	// the `if m.timer != nil` guard is a TOCTOU and a doubled Stop would
	// double-close stopCh.
	runMu  sync.Mutex
	timer  *time.Ticker
	stopCh chan struct{}
	onFire func(Fire)

	// firesWG tracks every in-flight onFire goroutine spawned by Tick (
	// switched to async dispatch so a slow turn never blocks subsequent ticks).
	// The daemon calls Wait before closing the store on shutdown so a
	// cron-fired turn isn't half-flushed when st.Close fires — same shape as
	// connector.WaitTurns + botTarget.turnsWG.
	firesWG sync.WaitGroup

	// loopWG tracks the scheduler loop goroutine itself so Stop can
	// synchronously wait for it to exit. Without this (finding),
	// `Stop` would return as soon as it closed stopCh, but the loop
	// goroutine could still be mid-`Tick` — and Tick is what increments
	// firesWG. Result: cm.Stop → cm.Wait race window in which Wait sees
	// firesWG=0 and returns, then the still-running Tick spawns a fire
	// goroutine, then connector.WaitTurns sets c.closed=true, then the fire
	// reaches enqueueTurn(closed) and SILENTLY drops the cron prompt.
	loopWG sync.WaitGroup

	// fireSync, when true, makes Tick invoke onFire on the caller goroutine
	// instead of dispatching to a new goroutine. Tests flip this so
	// `Tick; checkFireCount` works without a poll-wait. Production
	// always leaves it false so a slow turn never blocks subsequent ticks.
	fireSync bool
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
//
// Drops any persisted task whose CreatedBy isn't the new owner, in two
// scenarios:
//
// 1. In-process owner CHANGE (non-empty prior → different non-empty new):
// rotation/handoff while the daemon is running. the prior original case.
// 2. First-time owner resolution after RESTART (prior empty, persisted
// cron.json carries tasks whose CreatedBy != new owner): the operator
// restarted with a rotated bf_ token; the disk-loaded tasks are still
// authored by the prior owner and must not silently fire under the new
// owner..
//
// Rationale: an octo bot whose owner transfers — legitimate
// handoff OR an attacker who rotates the bf_ token and re-registers — would
// otherwise inherit every previously scheduled prompt. Those prompts fire
// with FromUID = old owner; for persona-OBO bots they fire `on_behalf_of`
// the OLD persona grantor, posting messages "on behalf of" someone who
// never consented.
func (m *Manager) SetOwnerUID(uid string) {
	// Refuse empty uid OUTRIGHT — don't even swap. An empty m.ownerUID
	// turns cron.Create's `RequestUID == OwnerUID` gate into
	// `"" == ""`, letting an unauthenticated control-bus caller create
	// tasks under the bot's identity (Sec). The only legitimate
	// callers are connector.OnOwner (fires only after a successful
	// BotRegisterResp with a non-empty server-resolved uid) and tests; both
	// already pass non-empty values, so a "" arriving here means a
	// malformed reconnect response or a future caller bug — fail closed.
	if uid == "" {
		return
	}
	// Two-phase swap: prune foreign-CreatedBy tasks FIRST under a probe of
	// the new uid, then commit the owner swap only if the prune succeeded.
	// The prior order (swap → prune) had a race window: Tick reading the
	// new ownerUID before s.mu allowed the prune to commit would fire
	// pre-prune tasks under the new persona (cross-tenant OBO replay). If
	// Update failed mid-rename, the owner stayed swapped but the foreign
	// tasks lived on, and every subsequent Tick fired them under the new
	// owner. Now: prune first → if the prune commits, swap; else keep
	// prior owner and surface the error.
	m.ownerMu.RLock()
	prior := m.ownerUID
	m.ownerMu.RUnlock()
	if prior == uid {
		return
	}
	var removed int
	_, err := m.store.Update(func(tasks []Task) ([]Task, bool) {
		out := make([]Task, 0, len(tasks))
		changed := false
		for _, t := range tasks {
			if t.CreatedBy == uid {
				out = append(out, t)
			} else {
				removed++
				changed = true
			}
		}
		return out, changed
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%scron: prune tasks before owner resolve %s→%s: %v (keeping prior owner)\n",
			m.label, prior, uid, err)
		return
	}
	m.ownerMu.Lock()
	m.ownerUID = uid
	m.ownerMu.Unlock()
	if removed > 0 {
		fmt.Fprintf(os.Stderr, "%scron: dropped %d task(s) on owner resolve %s→%s (foreign CreatedBy)\n",
			m.label, removed, prior, uid)
	}
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
// could otherwise assert the owner's uid). See core/cmd/octobuddy-daemon/control.go.
func (m *Manager) OwnerUID() string { return m.owner() }

// OnFire sets the callback invoked when a task is due (= the gateway's inbound
// path). Must be set before Start.
func (m *Manager) OnFire(fn func(Fire)) { m.onFire = fn }

// Start arms the periodic scan. Idempotent under concurrent calls; runs until
// Stop or until the loop's stop channel is closed.
func (m *Manager) Start() {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	if m.timer != nil {
		return
	}
	m.timer = time.NewTicker(CronTickInterval)
	m.stopCh = make(chan struct{})
	stopCh := m.stopCh
	timerCh := m.timer.C
	m.loopWG.Add(1)
	go func() {
		defer m.loopWG.Done()
		for {
			select {
			case <-stopCh:
				return
			case <-timerCh:
				m.Tick()
			}
		}
	}()
}

// Stop halts the scan and waits for the loop goroutine to exit. Once Stop
// returns, the loop is guaranteed not to run another Tick (and therefore
// not to enqueue any further onFire goroutines). Wait then drains the
// fires that DID get started. Without the loopWG.Wait here, a Tick that
// began before Stop's close-stopCh observation would still run to
// completion AFTER Stop returned, spawning fires that the caller's
// subsequent Wait couldn't see.
//
// loopWG.Wait runs INSIDE the runMu critical section: releasing runMu
// before the wait would let a concurrent Start observe a nilled timer +
// spawn a second loop goroutine while the first is still draining,
// producing two parallel Tick cycles that double-fire every due task.
// Tick does not acquire runMu, so holding it across the wait is safe.
func (m *Manager) Stop() {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	if m.timer == nil {
		return
	}
	m.timer.Stop()
	close(m.stopCh)
	m.timer = nil
	m.stopCh = nil
	m.loopWG.Wait()
}

// Wait blocks until every in-flight onFire goroutine spawned by Tick has
// returned. Call after Stop and BEFORE the daemon closes downstream resources
// (store, gateway) so a cron turn in mid-flush doesn't race the close.
// Idempotent: a manager that has never fired returns immediately.
func (m *Manager) Wait() { m.firesWG.Wait() }

// Tick performs one scan: fire due tasks, advance/drop them, persist. Exposed
// for tests. Never panics out — a failing onFire is logged and skipped, never
// crashing the loop. Mirrors cron-scheduler.ts tick: a single atomic
// read-modify-write fires due tasks and persists the survivor set in one pass,
// so a concurrent create/delete can't lose updates.
func (m *Manager) Tick() {
	// refuse to fire while no bot owner has been resolved
	// yet (connector.Run hasn't successfully registered with octo-server,
	// or the bot is between OwnerUID rotations). The SetOwnerUID prune
	// runs from OnOwner — between cm.Start and the first OnOwner callback
	// there's a window where a tick could fire persisted tasks under the
	// PRIOR owner's identity (F1 partial re-open). Holding fires
	// until owner is known closes that window. Tasks aren't lost: the
	// next Tick after OwnerUID arrives fires them normally.
	if m.OwnerUID() == "" {
		return
	}
	now := m.now()
	nowMS := unixMS(now)
	var fires []Fire
	_, err := m.store.Update(func(tasks []Task) ([]Task, bool) {
		survivors, dueFires, changed := collectCronFires(tasks, now, nowMS)
		// Defer the actual fire until after the store write returns, so onFire
		// (which drives a full turn) does not run while we hold the store mutex.
		fires = append(fires, dueFires...)
		return survivors, changed
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%scron: tick failed: %v\n", m.label, err)
		return
	}
	m.dispatchCronFires(fires, nowMS)
}

func collectCronFires(tasks []Task, now time.Time, nowMS int64) ([]Task, []Fire, bool) {
	survivors := make([]Task, 0, len(tasks))
	var fires []Fire
	changed := false
	for _, task := range tasks {
		if !isCronTaskDue(task, nowMS) {
			survivors = append(survivors, task)
			continue
		}
		changed = true
		fires = append(fires, Fire{Task: task})
		if nextTask, keep := advanceFiredCronTask(task, now, nowMS); keep {
			survivors = append(survivors, nextTask)
		}
		// one-shot: drop (not appended to survivors)
	}
	return survivors, fires, changed
}

func isCronTaskDue(task Task, nowMS int64) bool {
	return task.Enabled && task.NextRun != 0 && task.NextRun <= nowMS
}

func advanceFiredCronTask(task Task, now time.Time, nowMS int64) (Task, bool) {
	task.LastRun = nowMS
	if !task.Recurring {
		return task, false
	}
	// Compute the just-fired wall-clock key inline (not persisted — at-most-once
	// semantics, and a restart between Update-commit and the next due Tick
	// simply sees the already-advanced NextRun). The skip prevents
	// computeNextRunSkipping from re-scheduling the SAME wall-clock minute on DST
	// fall-back, where wall-clock 01:00-01:59 happens twice in absolute time.
	firedKey := fireKey(now)
	if next, ok := computeNextRunSkipping(task.Schedule, now, firedKey); ok {
		task.NextRun = unixMS(next)
		return task, true
	}
	// No future occurrence (e.g. a one-shot ISO time wrongly flagged recurring,
	// or an unsatisfiable cron): drop it rather than keeping a dead task that
	// never fires yet counts against MaxTasksPerBot (L27).
	return Task{}, false
}

func (m *Manager) dispatchCronFires(fires []Fire, nowMS int64) {
	for _, f := range fires {
		late := time.Duration(nowMS-f.Task.NextRun) * time.Millisecond
		if late >= time.Minute {
			fmt.Fprintf(os.Stderr, "%scron: task %s (%s) fired %d min late (catch-up)\n",
				m.label, f.Task.ID, f.Task.Schedule, int(late.Minutes()))
		}
		if m.onFire != nil {
			// Dispatch on its own goroutine so a long-running turn doesn't
			// block subsequent Tick calls (the timer's channel only buffers
			// one tick, so blocking here serialized every task in the bot
			// behind the slowest one — a daily 09:00 stack with one 8-min
			// task would fire the other four 8 min late). The gateway
			// already serializes per-session, so two recurring tasks in
			// the same session still queue cleanly.
			if m.fireSync {
				m.safeFire(f)
			} else {
				m.firesWG.Add(1)
				go func(f Fire) {
					defer m.firesWG.Done()
					m.safeFire(f)
				}(f)
			}
		}
	}
}

// safeFire invokes onFire, recovering from a panic so one bad fire never crashes
// the scheduler loop (cc-channel's best-effort guarantee).
func (m *Manager) safeFire(f Fire) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "%scron: onFire panicked for %s: %v\n", m.label, f.Task.ID, r)
		}
	}()
	m.onFire(f)
}

// unixMS converts a time to Unix milliseconds (the on-disk unit, matching
// cc-channel's Date.now).
func unixMS(t time.Time) int64 { return t.UnixNano() / int64(time.Millisecond) }
