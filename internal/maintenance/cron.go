// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package maintenance

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed 5-field cron expression (minute hour day-of-month month
// day-of-week). Every field is stored as a bitset of its allowed values.
type Cron struct {
	minutes  uint64 // bit n: minute n (0-59)
	hours    uint64 // bit n: hour n (0-23)
	days     uint64 // bit n: day of month n (1-31)
	months   uint64 // bit n: month n (1-12)
	weekdays uint64 // bit n: weekday n (0-6, 0 = Sunday)

	// domStar and dowStar record whether the field starts with '*'. As in
	// Vixie cron, the day fields are ANDed if either is starred and ORed if
	// both are restricted.
	domStar bool
	dowStar bool
}

// cronMacros maps the supported @-macros to their 5-field equivalents.
var cronMacros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

var (
	monthNames = map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}
	weekdayNames = map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}
)

// ParseCron parses a standard 5-field cron expression or one of the macros
// @yearly, @annually, @monthly, @weekly, @daily, @midnight and @hourly.
// Fields support *, numbers, names (JAN-DEC, SUN-SAT, case-insensitive),
// ranges a-b, lists a,b, and steps */n and a-b/n. Day-of-week accepts 0-7,
// where 0 and 7 both mean Sunday.
func ParseCron(expr string) (*Cron, error) {
	text := strings.ToLower(strings.TrimSpace(expr))
	if strings.HasPrefix(text, "@") {
		expanded, ok := cronMacros[text]
		if !ok {
			return nil, fmt.Errorf("cron %q: unknown macro", expr)
		}
		text = expanded
	}
	fields := strings.Fields(text)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields, got %d", expr, len(fields))
	}

	var c Cron
	var err error
	if c.minutes, _, err = parseField("minute", fields[0], 0, 59, nil); err != nil {
		return nil, err
	}
	if c.hours, _, err = parseField("hour", fields[1], 0, 23, nil); err != nil {
		return nil, err
	}
	if c.days, c.domStar, err = parseField("day-of-month", fields[2], 1, 31, nil); err != nil {
		return nil, err
	}
	if c.months, _, err = parseField("month", fields[3], 1, 12, monthNames); err != nil {
		return nil, err
	}
	if c.weekdays, c.dowStar, err = parseField("day-of-week", fields[4], 0, 7, weekdayNames); err != nil {
		return nil, err
	}
	// Sunday may be written as 7; fold it onto 0.
	if c.weekdays&(1<<7) != 0 {
		c.weekdays &^= 1 << 7
		c.weekdays |= 1
	}
	return &c, nil
}

// parseField parses a comma-separated list of parts for one field. It also
// reports whether the field starts with '*'.
func parseField(name, field string, lo, hi int, names map[string]int) (uint64, bool, error) {
	if field == "" {
		return 0, false, fmt.Errorf("cron %s field: empty", name)
	}
	var bits uint64
	for _, part := range strings.Split(field, ",") {
		partBits, err := parsePart(name, field, part, lo, hi, names)
		if err != nil {
			return 0, false, err
		}
		bits |= partBits
	}
	return bits, strings.HasPrefix(field, "*"), nil
}

// parsePart parses one element of a list: "*", "n", "a-b", optionally
// followed by "/step".
func parsePart(name, field, part string, lo, hi int, names map[string]int) (uint64, error) {
	rangeSpec, stepSpec, hasStep := strings.Cut(part, "/")
	step := 1
	if hasStep {
		n, ok := parseNumber(stepSpec)
		if !ok || n < 1 || n > hi-lo+1 {
			return 0, fmt.Errorf("cron %s field %q: invalid step %q", name, field, stepSpec)
		}
		step = n
	}

	var from, to int
	var err error
	switch {
	case rangeSpec == "*":
		from, to = lo, hi
	case strings.Contains(rangeSpec, "-"):
		a, b, _ := strings.Cut(rangeSpec, "-")
		if from, err = parseValue(name, field, a, lo, hi, names); err != nil {
			return 0, err
		}
		if to, err = parseValue(name, field, b, lo, hi, names); err != nil {
			return 0, err
		}
		if from > to {
			return 0, fmt.Errorf("cron %s field %q: range %q is reversed", name, field, rangeSpec)
		}
	default:
		if from, err = parseValue(name, field, rangeSpec, lo, hi, names); err != nil {
			return 0, err
		}
		to = from
		if hasStep {
			to = hi
		}
	}

	var bits uint64
	for v := from; v <= to; v += step {
		bits |= 1 << v
	}
	return bits, nil
}

// parseValue parses a single number or name and checks it against lo..hi.
func parseValue(name, field, s string, lo, hi int, names map[string]int) (int, error) {
	if v, ok := names[s]; ok {
		return v, nil
	}
	n, ok := parseNumber(s)
	if !ok {
		return 0, fmt.Errorf("cron %s field %q: %q is not a number or name", name, field, s)
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("cron %s field %q: value %d out of range %d-%d", name, field, n, lo, hi)
	}
	return n, nil
}

// parseNumber accepts only plain decimal digits.
func parseNumber(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// Matches reports whether the wall-clock minute of t, read in t's location,
// matches the expression.
func (c *Cron) Matches(t time.Time) bool {
	return c.matchesClock(t.Hour(), t.Minute()) && c.matchesDate(t.Month(), t.Day(), t.Weekday())
}

// matchesClock reports whether hour and minute are allowed.
func (c *Cron) matchesClock(hour, minute int) bool {
	return has(c.hours, hour) && has(c.minutes, minute)
}

// matchesDate reports whether a calendar day is allowed.
func (c *Cron) matchesDate(month time.Month, day int, weekday time.Weekday) bool {
	if !has(c.months, int(month)) {
		return false
	}
	domOK := has(c.days, day)
	dowOK := has(c.weekdays, int(weekday))
	if c.domStar || c.dowStar {
		return domOK && dowOK
	}
	return domOK || dowOK
}

// has reports whether bit n is set.
func has(bits uint64, n int) bool {
	return n >= 0 && n < 64 && bits&(1<<n) != 0
}
