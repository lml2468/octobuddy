package cron

import (
	"testing"
	"time"
)

// fixedLocal builds a local-zone time for cron-field matching tests.
func fixedLocal(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.Local)
}

func TestParseCronExpressionRejectsInvalid(t *testing.T) {
	invalid := []string{
		"not cron",
		"",
		"* * * *",     // 4 fields
		"* * * * * *", // 6 fields
		"60 * * * *",  // minute out of range
		"* 24 * * *",  // hour out of range
		"* * 0 * *",   // dom min is 1
		"* * * 13 *",  // month out of range
		"* * * * 7",   // dow max is 6
		"5-3 * * * *", // inverted range
		"0,,1 * * * *",
		"*/x * * * *",
		"*/0 * * * *",
		"x * * * *",
		"999999999999999999999999999999999 * * * *",
	}
	for _, e := range invalid {
		if parseCronExpression(e) != nil {
			t.Errorf("parseCronExpression(%q) should be invalid", e)
		}
	}
}

func TestParseCronExpressionAcceptsValid(t *testing.T) {
	valid := []string{"* * * * *", "0 9 * * 1-5", "*/15 * * * *", "0,30 * * * *", "5/10 * * * *", "1-5/2 * * * *"}
	for _, e := range valid {
		if parseCronExpression(e) == nil {
			t.Errorf("parseCronExpression(%q) should be valid", e)
		}
	}
}

func TestMatchesCron(t *testing.T) {
	star := parseCronExpression("* * * * *")
	if !star.matches(fixedLocal(2026, 6, 9, 13, 47)) {
		t.Error(`"* * * * *" should match any date`)
	}

	mon9 := parseCronExpression("0 9 * * 1") // Mon 09:00
	if !mon9.matches(fixedLocal(2026, 6, 8, 9, 0)) {
		t.Error("Mon 2026-06-08 09:00 should match")
	}
	if mon9.matches(fixedLocal(2026, 6, 8, 10, 0)) {
		t.Error("wrong hour should not match")
	}
	if mon9.matches(fixedLocal(2026, 6, 9, 9, 0)) {
		t.Error("Tuesday should not match")
	}

	step := parseCronExpression("*/15 * * * *")
	for _, m := range []int{0, 15, 30, 45} {
		if !step.matches(fixedLocal(2026, 6, 9, 10, m)) {
			t.Errorf("*/15 should match minute %d", m)
		}
	}
	if step.matches(fixedLocal(2026, 6, 9, 10, 7)) {
		t.Error("*/15 should not match minute 7")
	}
}

func TestMatchesCronDomDowOR(t *testing.T) {
	// "0 0 1 * 1" → midnight on the 1st OR any Monday.
	p := parseCronExpression("0 0 1 * 1")
	if !p.matches(fixedLocal(2026, 6, 1, 0, 0)) { // the 1st (also a Monday)
		t.Error("the 1st should match")
	}
	if !p.matches(fixedLocal(2026, 6, 8, 0, 0)) { // a Monday, not the 1st
		t.Error("a Monday should match")
	}
	if p.matches(fixedLocal(2026, 6, 9, 0, 0)) { // Tuesday, not the 1st
		t.Error("Tue not-the-1st should not match")
	}
}

func TestIsOneShotSchedule(t *testing.T) {
	cases := map[string]bool{
		"2026-06-09T09:00:00Z": true,
		"0 9 * * *":            false,
		"* * * * *":            false,
	}
	for in, want := range cases {
		if got := isOneShotSchedule(in); got != want {
			t.Errorf("isOneShotSchedule(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestComputeNextRunCron(t *testing.T) {
	now := time.Date(2026, 6, 9, 10, 0, 30, 0, time.Local)
	next, ok := computeNextRun("*/15 * * * *", now)
	if !ok {
		t.Fatal("expected a next run")
	}
	if !next.After(now) {
		t.Error("next run must be after now")
	}
	if next.Second() != 0 {
		t.Errorf("next run should be whole-minute, got sec=%d", next.Second())
	}
	m := next.Minute()
	if m != 0 && m != 15 && m != 30 && m != 45 {
		t.Errorf("next run minute %d not on the */15 grid", m)
	}
}

func TestComputeNextRunOneShot(t *testing.T) {
	now := time.Date(2026, 6, 9, 10, 0, 30, 0, time.UTC)

	future := "2999-01-01T00:00:00Z"
	next, ok := computeNextRun(future, now)
	if !ok {
		t.Fatal("future one-shot should return its instant")
	}
	want, _ := time.Parse(time.RFC3339, future)
	if !next.Equal(want) {
		t.Errorf("one-shot next = %v, want %v", next, want)
	}

	if _, ok := computeNextRun("2000-01-01T00:00:00Z", now); ok {
		t.Error("past one-shot should return none")
	}
}

func TestComputeNextRunInvalid(t *testing.T) {
	now := time.Date(2026, 6, 9, 10, 0, 30, 0, time.Local)
	if _, ok := computeNextRun("bogus expr here now", now); ok {
		t.Error("invalid cron should return none")
	}
	// Impossible schedule (Feb 31) must terminate the scan and return none.
	if _, ok := computeNextRun("0 0 31 2 *", now); ok {
		t.Error("Feb 31 should return none")
	}
}

func TestParseOneShotStrict(t *testing.T) {
	accept := []string{
		"2999-01-01T00:00:00Z",
		"2999-06-09T09:30Z",
		"2999-06-09T09:30:00",
		"2999-06-09T09:30:00+08:00",
		"2028-02-29T00:00:00+08:00", // 2028 is a leap year
	}
	for _, s := range accept {
		if _, ok := parseOneShot(s); !ok {
			t.Errorf("parseOneShot(%q) should accept", s)
		}
	}

	reject := []string{
		"2026-13-13T00:00:00Z",      // month 13
		"2026-02-31T00:00:00Z",      // Feb 31
		"2026-06-32T00:00:00Z",      // day 32
		"2026-02-31T00:00:00+08:00", // offset rollover
		"2026-13-13T00:00:00+05:30",
		"2026-06-32T00:00:00-07:00",
		"2026-02-31T00:00:00",       // no zone
		"2025-02-29T00:00:00+08:00", // 2025 not a leap year
		"2026-06-09T25:00:00+08:00", // hour 25
		"2026-06-09T00:61:00+08:00", // minute 61
		"T",
		"0T0",
		"badT123",
		"not a date",
	}
	for _, s := range reject {
		if _, ok := parseOneShot(s); ok {
			t.Errorf("parseOneShot(%q) should reject", s)
		}
	}
}

func TestComputeNextRunRejectsRolloverOneShot(t *testing.T) {
	if _, ok := computeNextRun("2026-13-13T00:00:00Z", time.Now()); ok {
		t.Error("computeNextRun must use strict one-shot parsing")
	}
}

func TestValidateSchedule(t *testing.T) {
	if !ValidateSchedule("0 9 * * 1-5") {
		t.Error("valid cron should validate")
	}
	if !ValidateSchedule("2999-01-01T00:00:00Z") {
		t.Error("valid one-shot should validate")
	}
	if ValidateSchedule("0 0 31 2 *") {
		// impossible-but-parseable cron: parses, so ValidateSchedule returns true
		// (impossibility is caught at computeNextRun time, mirroring cron-tool.ts).
		t.Log("impossible cron still parses (caught at next-run)")
	}
	if ValidateSchedule("nonsense") {
		t.Error("garbage should not validate")
	}
	if ValidateSchedule("2026-02-31T00:00:00Z") {
		t.Error("rollover one-shot should not validate")
	}
}

// TestComputeNextRunSkippingSkipsDuplicateWallclock is the regression for the
// DST fall-back double-fire bug: in a zone with DST, wall-clock 01:00-01:59
// occurs twice in absolute time. Without the skip, a "30 1 * * *" task fires
// at wall-01:30 of the FIRST pass, then computeNextRun finds wall-01:30 of
// the SECOND pass as the next match — same wall-clock minute, fired twice.
// Once with skipKey set the scanner advances past the duplicated key.
func TestComputeNextRunSkippingSkipsDuplicateWallclock(t *testing.T) {
	// Simulated "after firing at this absolute time" — the call site uses the
	// fired-at instant as `from`, so the next match must be strictly later
	// AND must NOT carry the same wall-clock key.
	from := time.Date(2026, 1, 15, 1, 30, 0, 0, time.UTC)
	skip := fireKey(from)

	next, ok := computeNextRunSkipping("30 1 * * *", from, skip)
	if !ok {
		t.Fatal("expected a next match for daily 01:30")
	}
	if fireKey(next) == skip {
		t.Fatalf("computeNextRunSkipping returned the skipped key: %s", fireKey(next))
	}
	// And the next instant must be at least the next day (not the same
	// wall-clock minute), so resume continuity holds even outside DST.
	if next.Day() == from.Day() {
		t.Fatalf("next fire should land on a different day, got %v from %v", next, from)
	}
}

// TestComputeNextRunRecurringAdvancesAfterSkip exercises the integration: the
// scheduler's loop calls computeNextRunSkipping(schedule, now, justFiredKey)
// and the result must be strictly after `now` in absolute time as well as a
// different wall-clock minute.
func TestComputeNextRunRecurringAdvancesAfterSkip(t *testing.T) {
	from := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	skip := fireKey(from)
	next, ok := computeNextRunSkipping("0 9 * * *", from, skip)
	if !ok {
		t.Fatal("daily 09:00 should always have a next match")
	}
	if !next.After(from) {
		t.Fatalf("next must be > from: from=%v next=%v", from, next)
	}
	if fireKey(next) == skip {
		t.Fatalf("skipped key leaked through: %s", skip)
	}
}
