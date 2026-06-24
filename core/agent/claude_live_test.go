package agent

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// spawnReadInit spawns the claude binary with args, feeds a probe prompt on
// stdin, and returns the parsed `system/init` stream-json line (the CLI emits
// it BEFORE any API call). No API spend: it reads the init line and kills the
// process. Skips the test when claude is absent, or when the binary/auth is
// unusable in this environment (no init line). Shared by the live wiring tests
// so the spawn + env + first-line-parse boilerplate lives in one place.
func spawnReadInit(t *testing.T, args []string) map[string]any {
	t.Helper()
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not on PATH; skipping live wiring check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, bin, args...)
	// Unreachable base URL + dummy key: if the process reaches the API call it
	// fails fast, but we only need the init line (emitted first) and kill after.
	// CLAUDE_CONFIG_DIR to a tempdir so we never touch real operator state.
	cmd.Env = append(cmd.Environ(),
		"ANTHROPIC_API_KEY=sk-ant-probe",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:9",
		"CLAUDE_CONFIG_DIR="+t.TempDir(),
	)
	cmd.Stdin = strings.NewReader("probe")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start claude: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	sc := newClaudeScanner(stdout)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["type"] == "system" && m["subtype"] == "init" {
			return m
		}
	}
	t.Skip("claude emitted no system/init line (auth/binary unusable in this env); skipping")
	return nil
}

// TestClaudeMinimalModeAppliesBypass is a live wiring check: it spawns the real
// claude binary with the driver's minimal-mode argv and asserts the binary
// actually APPLIED --permission-mode bypassPermissions. Argv-only tests
// (TestClaudeArgsMinimalMode) prove we PASS the flag; this proves the binary
// HONORS it — the regression guard for the headless invariant.
func TestClaudeMinimalModeAppliesBypass(t *testing.T) {
	// Seed the probe cache so buildArgs() doesn't itself spawn a probe; we only
	// care that minimal mode requests bypassPermissions.
	d := newTestDriver()
	args := d.buildArgs(Request{Prompt: "hi", SystemPrompt: "t"})

	init := spawnReadInit(t, args)
	if got, _ := init["permissionMode"].(string); got != "bypassPermissions" {
		t.Fatalf("minimal mode must run under bypassPermissions, got %q\ninit: %v", got, init)
	}
}

// TestProbeToolsReturnsTools is a live wiring check that exercises ProbeTools
// against the real binary: it asserts the returned tool surface is non-empty
// and includes the always-present Read/Bash. No API spend (ProbeTools reads
// the init line and kills the process). Skips when claude isn't on PATH.
func TestProbeToolsReturnsTools(t *testing.T) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not on PATH; skipping live probe check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tools, err := ProbeTools(ctx, bin, []string{
		"ANTHROPIC_API_KEY=sk-ant-probe",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:9",
		"CLAUDE_CONFIG_DIR=" + t.TempDir(),
	})
	if err != nil {
		t.Skipf("probe unusable in this env (%v); skipping", err)
	}
	has := func(name string) bool {
		for _, x := range tools {
			if x == name {
				return true
			}
		}
		return false
	}
	if !has("Read") || !has("Bash") {
		t.Fatalf("probe missing core tools (Read/Bash): %v", tools)
	}
}
