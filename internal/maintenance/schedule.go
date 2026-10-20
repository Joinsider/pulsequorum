// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package maintenance computes when maintenance windows are active
// (FA-MNT-01). It is pure: no I/O, no clock reads; callers pass the instants
// they ask about.
//
// A schedule is either "once", a fixed interval [StartsMs, EndsMs), or
// "recurring": a 5-field cron expression evaluated in a time zone. Every cron
// match starts a window that lasts DurationMinutes of elapsed time.
//
// DST rules for recurring schedules:
//   - Cron fields are matched against the local wall-clock time in the zone.
//   - A local start time that does not exist (spring-forward gap) starts at the
//     instant the clock jumps forward. In Europe/Berlin, "30 2 * * *" on
//     2026-03-29 starts at 01:00 UTC (03:00 CEST).
//   - A local start time that occurs twice (fall-back overlap) starts once, at
//     its first occurrence. In Europe/Berlin, "30 2 * * *" on 2026-10-25 starts
//     at 00:30 UTC (02:30 CEST); the second occurrence at 01:30 UTC starts
//     nothing.
//   - Durations are elapsed real time. A window crossing a transition lasts
//     DurationMinutes of real time, not of wall-clock time.
//
// StartsMs and EndsMs bound recurring starts (0 = unbounded). They do not cut
// the end of an occurrence that started within the bounds.
package maintenance

import (
	"fmt"
	"time"

	"github.com/joinsider/uptimecluster/internal/store"
)

const (
	// maxDurationMinutes is one week.
	maxDurationMinutes = 10080
	// searchYears bounds the NextStart search.
	searchYears = 5
	// dstSlack covers any zone offset and any single transition near a local time.
	dstSlack = 24 * time.Hour
)

// Schedule is a validated, compiled maintenance schedule.
type Schedule struct {
	once     bool
	onceFrom time.Time
	onceTo   time.Time

	cron     *Cron
	duration time.Duration
	loc      *time.Location
	startsMs int64
	endsMs   int64
}

// Compile validates s and returns its compiled form.
//
// For "once" the interval must satisfy StartsMs < EndsMs and Cron must be
// empty. For "recurring" the cron expression must parse, DurationMinutes must
// be in 1..10080, Timezone must load (empty means UTC), and, when both bounds
// are set, StartsMs < EndsMs.
func Compile(s store.MaintenanceSchedule) (*Schedule, error) {
	switch s.Kind {
	case store.ScheduleOnce:
		if s.Cron != "" {
			return nil, fmt.Errorf("maintenance: once schedule must not set cron %q", s.Cron)
		}
		if s.StartsMs >= s.EndsMs {
			return nil, fmt.Errorf("maintenance: once schedule needs startsMs < endsMs, got %d and %d", s.StartsMs, s.EndsMs)
		}
		return &Schedule{
			once:     true,
			onceFrom: time.UnixMilli(s.StartsMs).UTC(),
			onceTo:   time.UnixMilli(s.EndsMs).UTC(),
		}, nil

	case store.ScheduleRecurring:
		cron, err := ParseCron(s.Cron)
		if err != nil {
			return nil, fmt.Errorf("maintenance: recurring schedule: %w", err)
		}
		if s.DurationMinutes < 1 || s.DurationMinutes > maxDurationMinutes {
			return nil, fmt.Errorf("maintenance: durationMinutes must be 1..%d, got %d", maxDurationMinutes, s.DurationMinutes)
		}
		loc := time.UTC
		if s.Timezone != "" {
			loc, err = time.LoadLocation(s.Timezone)
			if err != nil {
				return nil, fmt.Errorf("maintenance: timezone %q: %w", s.Timezone, err)
			}
		}
		if s.StartsMs != 0 && s.EndsMs != 0 && s.StartsMs >= s.EndsMs {
			return nil, fmt.Errorf("maintenance: startsMs must be before endsMs, got %d and %d", s.StartsMs, s.EndsMs)
		}
		return &Schedule{
			cron:     cron,
			duration: time.Duration(s.DurationMinutes) * time.Minute,
			loc:      loc,
			startsMs: s.StartsMs,
			endsMs:   s.EndsMs,
		}, nil

	default:
		return nil, fmt.Errorf("maintenance: unknown schedule kind %q", s.Kind)
	}
}

// Occurrence returns the window active at at (start <= at < end). When
// recurring windows overlap, it returns the one with the latest end. The
// returned times are UTC.
func (s *Schedule) Occurrence(at time.Time) (start, end time.Time, ok bool) {
	if s.once {
		if !at.Before(s.onceFrom) && at.Before(s.onceTo) {
			return s.onceFrom, s.onceTo, true
		}
		return time.Time{}, time.Time{}, false
	}

	// A start st is a candidate if at-duration < st <= at. Local days are
	// scanned with slack on both sides because a local wall time can map to
	// an instant on an adjacent UTC day.
	first := civilDay(at.Add(-s.duration - 2*dstSlack).In(s.loc))
	last := civilDay(at.In(s.loc)).AddDate(0, 0, 1)
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		for _, st := range s.startsOn(day) {
			en := st.Add(s.duration)
			if st.After(at) || !at.Before(en) || !s.inBounds(st) {
				continue
			}
			if !ok || en.After(end) {
				start, end, ok = st, en, true
			}
		}
	}
	return start, end, ok
}

// NextStart returns the first window start strictly after after. It searches
// at most five years ahead and returns false if there is none. The returned
// time is UTC.
func (s *Schedule) NextStart(after time.Time) (time.Time, bool) {
	if s.once {
		if s.onceFrom.After(after) {
			return s.onceFrom, true
		}
		return time.Time{}, false
	}

	local := after.In(s.loc)
	first := civilDay(local).AddDate(0, 0, -1)
	last := civilDay(local).AddDate(searchYears, 0, 1)
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		// Starts are monotonic in local time, so the first one past the
		// end bound ends the search.
		for _, st := range s.startsOn(day) {
			if s.endsMs != 0 && st.UnixMilli() >= s.endsMs {
				return time.Time{}, false
			}
			if s.inBounds(st) && st.After(after) {
				return st, true
			}
		}
	}
	return time.Time{}, false
}

// inBounds reports whether a recurring start lies within StartsMs and EndsMs.
func (s *Schedule) inBounds(st time.Time) bool {
	ms := st.UnixMilli()
	if s.startsMs != 0 && ms < s.startsMs {
		return false
	}
	return s.endsMs == 0 || ms < s.endsMs
}

// startsOn returns the start instants of all matches on the civil day (a
// UTC-coded date), in ascending order.
func (s *Schedule) startsOn(day time.Time) []time.Time {
	if !s.cron.matchesDate(day.Month(), day.Day(), day.Weekday()) {
		return nil
	}
	var starts []time.Time
	for hour := 0; hour < 24; hour++ {
		for minute := 0; minute < 60; minute++ {
			if s.cron.matchesClock(hour, minute) {
				starts = append(starts, resolveLocal(day.Year(), day.Month(), day.Day(), hour, minute, s.loc))
			}
		}
	}
	return starts
}

// resolveLocal maps a wall-clock time in loc to the instant a window starts at.
// A nonexistent wall time maps to the moment of the forward jump. An ambiguous
// one maps to its first occurrence.
func resolveLocal(year int, month time.Month, day, hour, minute int, loc *time.Location) time.Time {
	wall := time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
	before := offsetAt(wall.Add(-dstSlack), loc)
	after := offsetAt(wall.Add(dstSlack), loc)

	var found []time.Time
	for _, off := range []int{before, after} {
		candidate := wall.Add(-time.Duration(off) * time.Second)
		if !sameWallClock(candidate.In(loc), wall) {
			continue
		}
		if len(found) == 0 || !found[0].Equal(candidate) {
			found = append(found, candidate)
		}
	}

	switch len(found) {
	case 0:
		return gapStart(wall, before, after, loc)
	case 1:
		return found[0]
	default:
		if found[1].Before(found[0]) {
			return found[1]
		}
		return found[0]
	}
}

// gapStart returns the first instant whose local time is at or after the
// nonexistent wall time, i.e. the instant of the forward jump. The offset is
// before at wall-dstSlack and after at wall+dstSlack; a binary search over
// whole seconds finds the first instant that has the after offset.
func gapStart(wall time.Time, before, after int, loc *time.Location) time.Time {
	if after <= before {
		// Not a forward jump, so there is no gap. Unreachable with tzdata;
		// fall back to the literal interpretation.
		return wall.Add(-time.Duration(after) * time.Second)
	}
	lo := wall.Add(-dstSlack).Unix()
	hi := wall.Add(dstSlack).Unix()
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		if offsetAt(time.Unix(mid, 0), loc) == after {
			hi = mid
		} else {
			lo = mid
		}
	}
	return time.Unix(hi, 0).UTC()
}

// offsetAt returns the UTC offset of loc at instant t, in seconds.
func offsetAt(t time.Time, loc *time.Location) int {
	_, off := t.In(loc).Zone()
	return off
}

// sameWallClock reports whether a and b show the same year, month, day, hour
// and minute, comparing their own fields.
func sameWallClock(a, b time.Time) bool {
	return a.Year() == b.Year() && a.Month() == b.Month() && a.Day() == b.Day() &&
		a.Hour() == b.Hour() && a.Minute() == b.Minute()
}

// civilDay returns the calendar date of t as a UTC midnight.
func civilDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
