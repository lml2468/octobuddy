package cron

import (
	"fmt"
	"os"
	"time"
)

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
