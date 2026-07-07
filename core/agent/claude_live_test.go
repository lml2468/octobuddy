package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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

	sc := newLineScanner(stdout)
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
// claude binary with the driver's argv and asserts the binary
// actually APPLIED --permission-mode bypassPermissions. Argv-only tests
// (TestClaudeArgsMinimalMode) prove we PASS the flag; this proves the binary
// HONORS it — the regression guard for the headless invariant.
func TestClaudeMinimalModeAppliesBypass(t *testing.T) {
	// Seed the probe cache so buildArgs() doesn't itself spawn a probe; we only
	// care that the turn requests bypassPermissions.
	d := newTestDriver()
	args := d.buildArgs(Request{Prompt: "hi", System: SystemPrompt{Persona: []string{"t"}}})

	init := spawnReadInit(t, args)
	if got, _ := init["permissionMode"].(string); got != "bypassPermissions" {
		t.Fatalf("must run under bypassPermissions, got %q\ninit: %v", got, init)
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

// echoMCPServer is a minimal stdio MCP server (initialize + tools/list + a
// single "ping" tool) used by the live MCP probe test. No network.
const echoMCPServer = `const send=(o)=>process.stdout.write(JSON.stringify(o)+"\n");let buf="";
process.stdin.on("data",(d)=>{buf+=d;let i;while((i=buf.indexOf("\n"))>=0){const line=buf.slice(0,i).trim();buf=buf.slice(i+1);if(!line)continue;let m;try{m=JSON.parse(line)}catch{continue}h(m)}});
function h(m){const{id,method}=m;if(method==="initialize")send({jsonrpc:"2.0",id,result:{protocolVersion:"2024-11-05",capabilities:{tools:{}},serverInfo:{name:"echo",version:"0"}}});
else if(method==="tools/list")send({jsonrpc:"2.0",id,result:{tools:[{name:"ping",description:"pong",inputSchema:{type:"object",properties:{}}}]}});
else if(method==="tools/call")send({jsonrpc:"2.0",id,result:{content:[{type:"text",text:"pong"}]}});
else if(id!==undefined)send({jsonrpc:"2.0",id,error:{code:-32601,message:"no"}})}`

// TestProbeMCPReportsHealth is a live wiring check: it writes a stub stdio MCP
// server + a .mcp.json, then asserts ProbeMCP reports the server connected and
// surfaces its mcp__echo__ping tool. A second config with a missing binary
// must report "failed". No API spend. Skips without claude or node on PATH.
func TestProbeMCPReportsHealth(t *testing.T) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not on PATH; skipping live MCP probe")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; skipping live MCP probe (stub server needs node)")
	}
	dir := t.TempDir()
	serverPath := filepath.Join(dir, "echo-mcp.mjs")
	if err := os.WriteFile(serverPath, []byte(echoMCPServer), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"ANTHROPIC_API_KEY=sk-ant-probe",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:9",
		"CLAUDE_CONFIG_DIR=" + filepath.Join(dir, "cfg"),
	}

	// Connected case.
	okCfg := filepath.Join(dir, "ok.mcp.json")
	writeMCP(t, okCfg, `{"mcpServers":{"echo":{"command":"node","args":[`+jsonStr(serverPath)+`]}}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	servers, err := ProbeMCP(ctx, bin, "", env, okCfg)
	if err != nil {
		t.Skipf("MCP probe unusable in this env (%v); skipping", err)
	}
	if len(servers) != 1 || servers[0].Name != "echo" {
		t.Fatalf("want one server 'echo', got %+v", servers)
	}
	if servers[0].Status != "connected" {
		t.Fatalf("echo should be connected, got %q", servers[0].Status)
	}
	if len(servers[0].Tools) == 0 {
		t.Fatalf("connected server should surface tools, got none: %+v", servers[0])
	}

	// Failed case (missing binary).
	badCfg := filepath.Join(dir, "bad.mcp.json")
	writeMCP(t, badCfg, `{"mcpServers":{"broken":{"command":"/nonexistent/xyz-octobuddy","args":[]}}}`)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()
	bad, err := ProbeMCP(ctx2, bin, "", env, badCfg)
	if err != nil {
		t.Fatalf("probe (bad cfg) errored: %v", err)
	}
	if len(bad) != 1 || bad[0].Status == "connected" {
		t.Fatalf("missing-binary server must not be connected, got %+v", bad)
	}
}

func writeMCP(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// jsonStr quotes s as a JSON string literal (for embedding a path in .mcp.json).
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
