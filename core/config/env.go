package config

// DriverEnv builds the KEY=VALUE environment to layer onto the claude CLI's
// process env: the user-declared agent.env, the model-gateway routing vars
// (mapped to the names claude understands), the octo-cli companion credential,
// and the per-bot CLAUDE_CONFIG_DIR isolation toggle. Tokens are supplied
// explicitly so the caller can pass runtime-injected values (from the in-memory
// secret store) rather than the config-file copies; empty strings omit the
// corresponding env var. secretValue resolves EnvValue.SecretRef entries from
// the active secret backend. Order matters: agent.env first, the named vars last —
// so the routing/credential injections always win over a same-named agent.env
// entry.
//
//	ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN
//	OCTO_BOT_TOKEN / OCTO_API_BASE_URL
//	CLAUDE_CONFIG_DIR (suppressed by agent.inheritUserConfig)
//
// Security note: the token is handed to the spawned `claude` child as an
// environment variable. On Linux that makes it readable from
// /proc/<pid>/environ by any same-uid process (and via `ps eww`), so the
// in-memory-only secret store's guarantee does not extend past the exec
// boundary. This is the accepted tradeoff documented in SECURITY.md — the
// agent CLI takes its credentials via env, and the daemon runs as the operator.
//
// octo-cli specifics: when OCTO_BOT_ID is set (the wizard always sets it),
// octo-cli does a DISK-PROFILE lookup keyed by robot id and IGNORES
// OCTO_BOT_TOKEN entirely — so the bf_ token alone in env isn't enough; the
// desktop side must also run `octo-cli auth login` per bot to write the disk
// profile (see desktop/internal/octocli.Login, called from configstore.Save).
// We still inject OCTO_BOT_TOKEN + OCTO_API_BASE_URL here as the fallback path
// for any agent code that bypasses --bot-id (e.g. a one-off `octo-cli api …`).
func (r Resolved) DriverEnv(gatewayToken, octoToken string, secretValue func(string) string) []string {
	out := agentEnvEntries(r.Agent.Env, secretValue)
	if r.Agent.GatewayBaseURL != "" {
		out = append(out, "ANTHROPIC_BASE_URL="+r.Agent.GatewayBaseURL)
	}
	if gatewayToken != "" {
		out = append(out, "ANTHROPIC_AUTH_TOKEN="+gatewayToken)
	}
	// octo-cli companion credential (appended last so it wins over any same-named
	// agent.env entry, mirroring the gateway vars above).
	if octoToken != "" {
		out = append(out, "OCTO_BOT_TOKEN="+octoToken)
	}
	if r.APIURL != "" {
		out = append(out, "OCTO_API_BASE_URL="+r.APIURL)
	}
	// Isolate the agent's config root from the operator's ~/.claude (user-scope
	// skills + installed plugins) unless explicitly told to inherit it. Auth is
	// env-based (above), so this is safe; built-in CLI skills still load.
	if r.ClaudeConfigDir != "" && !r.Agent.InheritUserConfig {
		out = append(out, "CLAUDE_CONFIG_DIR="+r.ClaudeConfigDir)
	}
	return out
}

func agentEnvEntries(env map[string]EnvValue, secretValue func(string) string) []string {
	var out []string
	for k, ev := range env {
		v, ok := resolveEnvValue(ev, secretValue)
		if ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

func resolveEnvValue(ev EnvValue, secretValue func(string) string) (string, bool) {
	if ev.SecretRef == "" {
		return ev.Value, true
	}
	if secretValue == nil {
		return "", false
	}
	v := secretValue(ev.SecretRef)
	return v, v != ""
}
