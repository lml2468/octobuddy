//go:build windows

package agent

import (
	"os/exec"
)

// setProcessGroup is a no-op on Windows: process-group semantics differ
// (job objects, not POSIX pgids), and the daemon's primary target is mac/linux.
// Without a job object, a killed claude can still orphan its MCP server
// children on Windows — acceptable for now; revisit if Windows MCP ships.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup falls back to killing just the direct child on Windows.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
