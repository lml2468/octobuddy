package octo

import "sync"

// seenSet is a bounded in-memory set of recently-processed inbound message ids,
// the first tier of inbound dedup (P1 / Efficiency #1). It sits on the socket
// read loop (checked in onInbound) so a same-process redelivery — a lost ack or
// a reconnect replay — is dropped with an O(1) map probe and NO database I/O,
// keeping the read loop (which also drives keepalive/DISCONNECT detection) off
// the SQLite write path. The authoritative cross-restart dedup is the second
// tier (store-backed, on the turn worker goroutine).
//
// Bounded like the socket's decryptFails map: past maxSeenKeys the oldest
// insertion is evicted. Eviction can only lose dedup for an id not seen within
// the last maxSeenKeys messages — far past any redelivery window — so a redroped
// id at worst causes one duplicate, which the persistent second tier still
// catches for the turn path.
type seenSet struct {
	mu    sync.Mutex
	set   map[string]struct{}
	order []string // insertion order for FIFO eviction
	cap   int
}

func newSeenSet(capacity int) *seenSet {
	return &seenSet{set: make(map[string]struct{}, capacity), cap: capacity}
}

// markSeen records id and reports whether this is the FIRST sighting (true =
// process it; false = duplicate, drop). Safe for concurrent use.
func (s *seenSet) markSeen(id string) (firstSeen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.set[id]; ok {
		return false
	}
	s.set[id] = struct{}{}
	s.order = append(s.order, id)
	for len(s.order) > s.cap {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.set, oldest)
	}
	return true
}
