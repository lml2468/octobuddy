// Package cron implements per-bot scheduled tasks — a faithful Go port of
// cc-channel-octo's #115 cron feature (cron-evaluator.ts, cron-store.ts,
// cron-scheduler.ts, cron-tool.ts). A scheduled task fires its stored prompt as
// a synthetic router.InboundMessage with CronFire=true, so it runs through the
// normal turn pipeline while bypassing the group @mention gate and rate limit
// (the router already honors CronFire; authenticity is guaranteed by the fire
// being in-process, the Go analogue of cc-channel's per-process nonce marker).
//
// This file is the evaluator: pure schedule math, no I/O (port of
// cron-evaluator.ts). Two schedule forms are supported:
//
// - 5-field cron "minute hour dom month dow" — each field is `*`, an integer,
// `*`+`/step`, an `a-b` range, or a comma list of those. Standard Unix
// semantics: dom/dow are OR'd when BOTH are restricted (fires when either
// matches), matching cron's historical behavior.
// - one-shot ISO datetime "2026-06-09T09:00:00Z" — fires once, then never.
//
// TIMEZONE: cron fields match against the injected clock's LOCAL time, so
// "0 9 * * *" means 9am in the server's timezone. One-shot ISO datetimes are
// absolute instants (honor any offset/`Z`).
package cron

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// parsedCron is a parsed 5-field cron expression: each field is the set of
// allowed values, plus whether dom/dow were restricted (affects OR semantics).
type parsedCron struct {
	minute        map[int]bool
	hour          map[int]bool
	dom           map[int]bool
	month         map[int]bool
	dow           map[int]bool // 0 = Sunday
	domRestricted bool
	dowRestricted bool
}

// fieldRange is a field's inclusive [min,max].
type fieldRange struct{ min, max int }

// fieldRanges order: minute, hour, dom, month, dow (port of cron-evaluator.ts).
var fieldRanges = [5]fieldRange{
	{0, 59}, // minute
	{0, 23}, // hour
	{1, 31}, // dom
	{1, 12}, // month
	{0, 6},  // dow (0 = Sunday)
}

var digitsRE = regexp.MustCompile(`^\d+$`)
var rangeRE = regexp.MustCompile(`^(\d+)-(\d+)$`)

// parseField expands one cron field into a set of allowed integers, or nil if
// invalid. Mirrors parseField in cron-evaluator.ts.
func parseField(raw string, min, max int) map[int]bool {
	out := map[int]bool{}
	for _, part := range strings.Split(raw, ",") {
		seg := strings.TrimSpace(part)
		if seg == "" {
			return nil
		}
		// step: "*/n" or "a-b/n" or "a/n"
		rangeStr := seg
		var stepStr string
		hasStep := false
		if slash := strings.IndexByte(seg, '/'); slash != -1 {
			rangeStr = seg[:slash]
			stepStr = seg[slash+1:]
			hasStep = true
		}
		step := 1
		if hasStep {
			if !digitsRE.MatchString(stepStr) {
				return nil
			}
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return nil
			}
			step = n
		}
		var lo, hi int
		switch {
		case rangeStr == "*":
			lo, hi = min, max
		case digitsRE.MatchString(rangeStr):
			n, err := strconv.Atoi(rangeStr)
			if err != nil {
				return nil
			}
			lo, hi = n, n
			// A bare number with a step (e.g. "5/10") means "from 5 to max, step".
			if hasStep {
				hi = max
			}
		default:
			m := rangeRE.FindStringSubmatch(rangeStr)
			if m == nil {
				return nil
			}
			lo, _ = strconv.Atoi(m[1])
			hi, _ = strconv.Atoi(m[2])
		}
		if lo < min || hi > max || lo > hi {
			return nil
		}
		for v := lo; v <= hi; v += step {
			out[v] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseCronExpression parses a 5-field cron expression. Returns nil when invalid.
func parseCronExpression(expr string) *parsedCron {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return nil
	}
	sets := make([]map[int]bool, 5)
	for i := range 5 {
		set := parseField(fields[i], fieldRanges[i].min, fieldRanges[i].max)
		if set == nil {
			return nil
		}
		sets[i] = set
	}
	return &parsedCron{
		minute:        sets[0],
		hour:          sets[1],
		dom:           sets[2],
		month:         sets[3],
		dow:           sets[4],
		domRestricted: fields[2] != "*",
		dowRestricted: fields[4] != "*",
	}
}

// matches reports whether t (local time) matches the parsed cron. Mirrors
// matchesCron in cron-evaluator.ts, including the dom/dow OR semantics.
func (p *parsedCron) matches(t time.Time) bool {
	if !p.minute[t.Minute()] {
		return false
	}
	if !p.hour[t.Hour()] {
		return false
	}
	if !p.month[int(t.Month())] {
		return false
	}
	domOk := p.dom[t.Day()]
	dowOk := p.dow[int(t.Weekday())] // time.Weekday: Sunday=0, matching cron
	// Standard cron OR semantics: if BOTH dom and dow are restricted, match when
	// EITHER matches; otherwise both (the unrestricted one is always true) must.
	if p.domRestricted && p.dowRestricted {
		return domOk || dowOk
	}
	return domOk && dowOk
}

// isOneShotSchedule heuristically reports whether schedule is a one-shot ISO
// datetime (vs a cron expr): ISO datetimes contain 'T' and no whitespace.
func isOneShotSchedule(schedule string) bool {
	s := strings.TrimSpace(schedule)
	return strings.Contains(s, "T") && !strings.ContainsAny(s, " \t\r\n")
}

// maxScanMinutes bounds the cron next-run scan (~366 days), so an impossible
// schedule (e.g. Feb 31) terminates rather than looping forever.
const maxScanMinutes = 366 * 24 * 60

// computeNextRun returns the next fire time strictly after from, or ok=false
// when there is none (a past/invalid one-shot, or an impossible cron). Mirrors
// computeNextRun in cron-evaluator.ts.
//
// - one-shot ISO: its instant if still in the future, else none.
// - cron: scan minute-by-minute from the next whole minute, up to ~366 days.
func computeNextRun(schedule string, from time.Time) (time.Time, bool) {
	return computeNextRunSkipping(schedule, from, "")
}

// computeNextRunSkipping is computeNextRun plus a "skip this wall-clock minute"
// guard. Used by the scheduler after firing a recurring task to skip the
// just-fired wall-clock minute — without which a DST fall-back ambiguous hour
// would fire the same minute twice (the second absolute-time pass through
// wall-01:30 would match the same cron expr that just fired at the first).
// skipKey is a "YYYY-MM-DDTHH:MM" formatted in the cursor's local zone, or ""
// to disable the skip (the create-path call).
func computeNextRunSkipping(schedule string, from time.Time, skipKey string) (time.Time, bool) {
	if isOneShotSchedule(schedule) {
		t, ok := parseOneShot(schedule)
		if !ok {
			return time.Time{}, false
		}
		if t.After(from) {
			return t, true
		}
		return time.Time{}, false
	}
	parsed := parseCronExpression(schedule)
	if parsed == nil {
		return time.Time{}, false
	}
	// Start at the next whole minute boundary after from, in the clock's zone.
	cursor := from.Truncate(time.Minute).Add(time.Minute)
	for range maxScanMinutes {
		if parsed.matches(cursor) && fireKey(cursor) != skipKey {
			return cursor, true
		}
		cursor = cursor.Add(time.Minute)
	}
	return time.Time{}, false // impossible schedule
}

// fireKey returns the wall-clock "YYYY-MM-DDTHH:MM" identifying a fire
// occurrence. Two distinct absolute times can share a key during DST fall-back;
// that's precisely the ambiguity computeNextRunSkipping is built to dedup.
func fireKey(t time.Time) string {
	return t.Format("2006-01-02T15:04")
}

// ValidateSchedule reports whether schedule is a parseable cron expr or a
// strictly-valid one-shot ISO datetime (used by the create path before storing).
func ValidateSchedule(schedule string) bool {
	if isOneShotSchedule(schedule) {
		_, ok := parseOneShot(schedule)
		return ok
	}
	return parseCronExpression(schedule) != nil
}
