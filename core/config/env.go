package config

// AgentEnvExtra resolves the user-declared per-bot agent.env into KEY=VALUE
// entries (plain values pass through; secretRef entries are resolved from the
// active secret backend, and an unresolved ref is omitted). This is the
// DRIVER-NEUTRAL part of the spawn environment — it names nothing
// driver-specific. The daemon (the join-point that imports core/agent) combines
// it with the gateway/octo/config-dir scalars into an agent.EnvSpec and hands
// that to the driver's EnvMapper, which owns the concrete var names
// (ANTHROPIC_*/CLAUDE_CONFIG_DIR for Claude). Keeping the naming behind the
// driver is what lets config stay a leaf with no agent import.
//
// Order among the agent.env entries is unspecified (map iteration); the daemon
// appends the named routing/credential vars AFTER these, so an injection wins
// over a same-named agent.env entry regardless.
//
// Security note: the gateway token ultimately reaches the spawned CLI as an
// environment variable. On Linux that makes it readable from /proc/<pid>/environ
// by any same-uid process; this is the accepted tradeoff documented in
// SECURITY.md — the agent CLI takes credentials via env and the daemon runs as
// the operator.
func (r Resolved) AgentEnvExtra(secretValue func(string) string) []string {
	var out []string
	for k, ev := range r.Agent.Env {
		if v, ok := resolveEnvValue(ev, secretValue); ok {
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
