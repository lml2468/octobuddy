package agent

// ParseLineForReplay exposes the Claude stream-json line normalizer for offline
// replay of recorded fixtures. It is the same logic the live driver uses.
func ParseLineForReplay(line string) []AgentEvent {
	return parseClaudeLine(line)
}
