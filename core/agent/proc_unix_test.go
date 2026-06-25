//go:build !windows

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestKillProcessGroupReapsGrandchild is the regression for the "probe doesn't
// exit" leak: claude spawns MCP servers as its own children, so killing only
// the direct child orphans them. killProcessGroup must tear down the WHOLE
// group. We model claude with a shell that backgrounds a long-lived grandchild
// and writes its pid, then assert the grandchild is dead after the group kill.
func TestKillProcessGroupReapsGrandchild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	// Parent backgrounds `sleep 300` (the stand-in MCP server), records its pid,
	// then waits — so the grandchild outlives a parent-only kill.
	script := "sleep 300 & echo $! > " + pidFile + "; wait"

	cmd := exec.Command("/bin/sh", "-c", script)
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Wait for the grandchild pid to be recorded.
	var gpid int
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(pidFile); err == nil && len(b) > 0 {
			if n, err := strconv.Atoi(string(b[:len(b)-1])); err == nil {
				gpid = n
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if gpid == 0 {
		killProcessGroup(cmd)
		_ = cmd.Wait()
		t.Fatal("grandchild pid never recorded")
	}

	killProcessGroup(cmd)
	_ = cmd.Wait()

	// After the group kill the grandchild must be gone. signal 0 probes liveness.
	for i := 0; i < 100; i++ {
		if err := syscall.Kill(gpid, 0); err != nil {
			return // ESRCH — grandchild reaped, leak fixed
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Clean up the leak this test just proved, so it doesn't linger.
	_ = syscall.Kill(gpid, syscall.SIGKILL)
	t.Fatalf("grandchild pid %d survived the process-group kill (leak)", gpid)
}
