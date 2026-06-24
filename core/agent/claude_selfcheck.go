package agent

import (
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/lml2468/octobuddy/core/clog"
)

// logSelfcheck emits one line summarizing the realized invocation environment
// for the bot's first turn. Greppable and paste-able for support tickets.
//
//	[selfcheck] bot=<id> claude=<path-or-MISSING> auth=<masked-or-UNSET>
//	            base_url=<url> cwd=<path> writable=<bool>
//	            mode=<minimal|claude_code> tools=<count-or-DEFAULT>
//
// `tools` reflects what `--tools` actually carried for this turn (req's
// override, or the driver default in minimal mode). In claude_code mode
// the field is "BYPASS" since --tools is not passed (bypassPermissions
// grants every tool).
func (d *ClaudeDriver) logSelfcheck(env []string, req Request) {
	envMap := map[string]string{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			envMap[kv[:i]] = kv[i+1:]
		}
	}
	bin := d.binPath()
	binStr := bin
	if p, err := exec.LookPath(bin); err == nil {
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
	clog.For("selfcheck").Info("driver invocation environment",
		"bot", botID, "claude", binStr, "auth", auth, "base_url", baseURL,
		"cwd", req.Cwd, "writable", isDirWritable(req.Cwd),
		"mode", string(d.mode()), "tools", d.selfcheckToolsField(req.AllowedTools))
}

// selfcheckToolsField reports the tool surface as it actually reaches the
// spawned CLI on this turn. "BYPASS" in claude_code mode flags that no
// whitelist applies; "NONE" for an explicit empty list; "probed:N" when the
// nil request resolved to the binary's headless-safe set; "CLI-DEFAULT" when
// the probe was unavailable (the CLI's own default set is used); otherwise the
// explicit override count.
func (d *ClaudeDriver) selfcheckToolsField(override []string) string {
	if d.mode() == PromptModeClaudeCode {
		return "BYPASS"
	}
	if override == nil {
		if safe := d.headlessTools(); len(safe) > 0 {
			return "probed:" + strconv.Itoa(len(safe))
		}
		return "CLI-DEFAULT"
	}
	if len(override) == 0 {
		return "NONE"
	}
	return strconv.Itoa(len(override))
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
