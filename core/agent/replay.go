package agent

// ParseLineForReplay exposes the Claude stream-json line normalizer for offline
// replay of recorded fixtures. With plain stream-json (no --include-partial-
// messages) there is no driver-private dedup sentinel to strip, so this is a
// thin pass-through kept as the stable name for replay consumers.
func ParseLineForReplay(line string) []AgentEvent {
	return parseClaudeLine(line)
}
