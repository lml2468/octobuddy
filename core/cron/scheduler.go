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
	// Empty owner would open the create gate ("" == ""), so fail closed.
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
