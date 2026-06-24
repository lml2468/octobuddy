package cron

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// oneShotRE matches YYYY-MM-DDThh:mm(:ss(.fff)?)? with optional Z or ±hh:mm.
var oneShotRE = regexp.MustCompile(
	`^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2})(?:\.\d{1,3})?)?(Z|[+-]\d{2}:\d{2})?$`)

// parseOneShot strictly parses a one-shot ISO-8601 datetime → time, or ok=false
// if invalid. Mirrors parseOneShot in cron-evaluator.ts: it requires the
// canonical shape AND verifies the authored wall-clock fields name a real
// calendar instant (so an out-of-range month/day/hour can't sneak through as a
// silently shifted time, for ALL zone forms — Z, ±hh:mm, or none).
func parseOneShot(schedule string) (time.Time, bool) {
	s := strings.TrimSpace(schedule)
	m := oneShotRE.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	yr := atoi(m[1])
	mo := atoi(m[2])
	day := atoi(m[3])
	hh := atoi(m[4])
	mm := atoi(m[5])
	ss := 0
	if m[6] != "" {
		ss = atoi(m[6])
	}
	// Calendar validity (is "Feb 31" real? is hour 25 valid?) is INDEPENDENT of
	// the timezone, so validate the authored wall-clock fields with a zone-free
	// UTC round-trip probe: if time.Date had to normalize any field, the rendered
	// field won't match what the user wrote. This catches offset rollover too
	// (e.g. 2026-02-31T00:00:00+08:00, which would otherwise roll into March).
	if !validWallClock(yr, mo, day, hh, mm, ss) {
		return time.Time{}, false
	}
	// Parse the actual instant honoring the zone. The zone-less form is the local
	// wall clock; Z/offset forms are absolute (RFC3339).
	if m[7] == "" {
		return time.Date(yr, time.Month(mo), day, hh, mm, ss, 0, time.Local), true
	}
	t, err := time.Parse(time.RFC3339, normalizeRFC3339(m))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func validWallClock(yr, mo, day, hh, mm, ss int) bool {
	probe := time.Date(yr, time.Month(mo), day, hh, mm, ss, 0, time.UTC)
	return probe.Year() == yr &&
		int(probe.Month()) == mo &&
		probe.Day() == day &&
		probe.Hour() == hh &&
		probe.Minute() == mm &&
		probe.Second() == ss
}

// normalizeRFC3339 renders the matched groups with an explicit seconds field
// (time.Parse with RFC3339 requires ss), so "2026-06-09T09:30Z" parses.
func normalizeRFC3339(m []string) string {
	sec := m[6]
	if sec == "" {
		sec = "00"
	}
	return m[1] + "-" + m[2] + "-" + m[3] + "T" + m[4] + ":" + m[5] + ":" + sec + m[7]
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
