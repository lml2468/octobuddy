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
// Backed by a fixed ring buffer of ids plus a membership map: one allocation for
// the buffer's lifetime, O(1) insert, and an evicted id's string is overwritten
// (not left pinned in a growing backing array). Eviction is insertion-FIFO —
// past cap the oldest id is overwritten, which can only lose dedup for an id not
// seen within the last cap messages, far past any redelivery window; a rare
// re-dropped id costs one duplicate, which the persistent second tier still
// catches for the turn path.
type seenSet struct {
	mu   sync.Mutex
	set  map[string]struct{}
	ring []string // fixed-size (cap) ring of ids in insertion order
	next int      // write index; wraps at len(ring)
	size int      // live entries (<= cap), so a not-yet-full ring evicts nothing
}

func newSeenSet(capacity int) *seenSet {
	if capacity < 1 {
		capacity = 1
	}
	return &seenSet{set: make(map[string]struct{}, capacity), ring: make([]string, capacity)}
}

// markSeen records id and reports whether this is the FIRST sighting (true =
// process it; false = duplicate, drop). Safe for concurrent use.
func (s *seenSet) markSeen(id string) (firstSeen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.set[id]; ok {
		return false
	}
	if s.size == len(s.ring) {
		// Full: overwrite (and forget) the oldest id at the write cursor.
		delete(s.set, s.ring[s.next])
	} else {
		s.size++
	}
	s.ring[s.next] = id
	s.next = (s.next + 1) % len(s.ring)
	s.set[id] = struct{}{}
	return true
}
