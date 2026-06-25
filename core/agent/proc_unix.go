//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// setProcessGroup makes the spawned process a process-group LEADER (pgid ==
// pid) so the whole tree it spawns — claude plus the MCP server subprocesses it
// launches as its own children — can be signalled as a unit. Without this, a
// later cmd.Process.Kill() reaps only claude and the MCP servers orphan to init
// and run forever (the "probe doesn't exit" leak).
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup SIGKILLs the entire process group led by cmd (negative pid =
// the group), tearing down claude AND every MCP server it spawned. Best-effort:
// a process that already exited, or that escaped its group, is simply not
// reaped here. Falls back to killing just the leader if the group send fails.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
