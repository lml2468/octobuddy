package agent

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// logSelfcheck emits one line summarizing the realized invocation environment
// for the bot's first turn. The shape is deliberately greppable + paste-able:
//
//	[selfcheck] bot=<id> claude=<path-or-MISSING:err> auth=<masked-or-UNSET> base_url=<url-or-UNSET> cwd=<path> writable=<true|false>
//
// The token is masked (first 6 + last 4) so the line is safe to paste into a
// support ticket without leaking the live key. claude=MISSING screams when the
// CLI isn't installed/on PATH; auth=UNSET screams when the gateway-token
// secret never made it into env (the actual root cause of the "出错了" report
// we got from a fresh install). Anything else worth knowing — workspace cwd
// not writable, custom base URL pointed at the wrong host — fits on the line.
func (d *ClaudeDriver) logSelfcheck(env []string, cwd string) {
	envMap := map[string]string{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			envMap[kv[:i]] = kv[i+1:]
		}
	}
	binStr := d.Bin
	if p, err := exec.LookPath(d.Bin); err == nil {
		binStr = p
	} else {
		binStr = "MISSING:" + err.Error()
	}
	auth := maskToken(envMap["ANTHROPIC_AUTH_TOKEN"])
	baseURL := envMap["ANTHROPIC_BASE_URL"]
	if baseURL == "" {
		baseURL = "UNSET"
	}
	botID := envMap["OCTO_BOT_ID"]
	if botID == "" {
		botID = "?"
	}
	writable := isDirWritable(cwd)
	fmt.Fprintf(os.Stderr, "[selfcheck] bot=%s claude=%s auth=%s base_url=%s cwd=%s writable=%t\n",
		botID, binStr, auth, baseURL, cwd, writable)
}

// maskToken returns a redacted form safe to log: "UNSET" if empty, the literal
// value if too short to mask meaningfully (< 10 chars), or first-6 + "..." +
// last-4 otherwise. Preserves enough surface for the operator to recognize
// which token is in play without exposing the secret.
func maskToken(s string) string {
	if s == "" {
		return "UNSET"
	}
	if len(s) < 10 {
		return "SHORT(" + s + ")"
	}
	return s[:6] + "..." + s[len(s)-4:]
}

// isDirWritable probes write access via a no-op .write-test create+remove. The
// claude CLI writes session state under CLAUDE_CONFIG_DIR and project files
// under cwd; a read-only mount (or wrong-owner dir after a HOME override)
// reproduces as a turn that fails immediately. Best-effort: returns false on
// any error including "the dir doesn't exist".
func isDirWritable(dir string) bool {
	if dir == "" {
		return false
	}
	probe := dir + "/.octobuddy-writetest"
	f, err := os.OpenFile(probe, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return true
}
