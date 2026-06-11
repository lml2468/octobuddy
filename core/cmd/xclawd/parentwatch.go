package main

import (
	"os"
	"time"
)

// watchParentExit invokes onOrphan once the parent process dies. On Unix an
// orphaned child is reparented to init (pid 1), so getppid() == 1 means the
// launcher that spawned us (the GUI app) is gone. The GUI passes
// -exit-with-parent so the embedded daemon never outlives the app, even on a
// crash or force-quit where graceful shutdown (control-bus stop) never ran.
//
// No-op if we are already a direct child of init (e.g. launched by launchd or
// systemd as a real daemon) — there is no app parent to track in that case.
func watchParentExit(onOrphan func()) {
	if os.Getppid() == 1 {
		return
	}
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for range t.C {
			if os.Getppid() == 1 {
				onOrphan()
				return
			}
		}
	}()
}
