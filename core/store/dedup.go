package store

import (
	"fmt"
	"time"
)

// MarkSeenMessage records messageID in the inbound-dedup set and reports whether
// this is the FIRST sighting. firstSeen=true means the caller should process the
// message; false means it's a duplicate redelivery and the caller should drop it
// (after still acking, so the server stops resending). Atomic via INSERT OR
// IGNORE + RowsAffected, so concurrent first-sightings can't both win.
func (s *Store) MarkSeenMessage(messageID string) (firstSeen bool, err error) {
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO seen_messages(message_id, seen_at) VALUES(?, ?)`,
		messageID, s.now().Unix())
	if err != nil {
		return false, fmt.Errorf("mark seen message: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark seen message rows: %w", err)
	}
	return n > 0, nil
}

// ReapSeenMessages deletes dedup rows older than olderThan, bounding the table.
// Called from the daemon's periodic reaper alongside router.Reap. Returns the
// number of rows removed.
func (s *Store) ReapSeenMessages(olderThan time.Duration) (int64, error) {
	cutoff := s.now().Add(-olderThan).Unix()
	res, err := s.db.Exec(`DELETE FROM seen_messages WHERE seen_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("reap seen messages: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
