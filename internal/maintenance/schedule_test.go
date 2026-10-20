// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package maintenance

import (
	"testing"
	"time"
	_ "time/tzdata" // deterministic zone data independent of the host

	"github.com/joinsider/uptimecluster/internal/store"
)

// occCase expects Occurrence(at) to return [start, end); an empty start means no occurrence.
type occCase struct {
	at, start, end string
}

// nextCase expects NextStart(after) to return want; an empty want means no start.
type nextCase struct {
	after, want string
}

func recurring(expr string, minutes int, tz string) store.MaintenanceSchedule {
	return store.MaintenanceSchedule{Kind: store.ScheduleRecurring, Cron: expr, DurationMinutes: minutes, Timezone: tz}
}

func mustCompile(t *testing.T, s store.MaintenanceSchedule) *Schedule {
	t.Helper()
	sch, err := Compile(s)
	if err != nil {
		t.Fatalf("Compile(%+v): %v", s, err)
	}
	return sch
}

func runOccurrence(t *testing.T, sch *Schedule, cases []occCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.at, func(t *testing.T) {
			start, end, ok := sch.Occurrence(mustParseTime(t, tc.at))
			if tc.start == "" {
				if ok {
					t.Fatalf("Occurrence(%s) = [%s, %s), want none", tc.at, start, end)
				}
				return
			}
			if !ok {
				t.Fatalf("Occurrence(%s) = none, want [%s, %s)", tc.at, tc.start, tc.end)
			}
			if want := mustParseTime(t, tc.start); !start.Equal(want) {
				t.Errorf("Occurrence(%s) start = %s, want %s", tc.at, start.Format(time.RFC3339), tc.start)
			}
			if want := mustParseTime(t, tc.end); !end.Equal(want) {
				t.Errorf("Occurrence(%s) end = %s, want %s", tc.at, end.Format(time.RFC3339), tc.end)
			}
		})
	}
}

func runNext(t *testing.T, sch *Schedule, cases []nextCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.after, func(t *testing.T) {
			got, ok := sch.NextStart(mustParseTime(t, tc.after))
			if tc.want == "" {
				if ok {
					t.Fatalf("NextStart(%s) = %s, want none", tc.after, got.Format(time.RFC3339))
				}
				return
			}
			if !ok {
				t.Fatalf("NextStart(%s) = none, want %s", tc.after, tc.want)
			}
			if want := mustParseTime(t, tc.want); !got.Equal(want) {
				t.Errorf("NextStart(%s) = %s, want %s", tc.after, got.Format(time.RFC3339), tc.want)
			}
		})
	}
}

func TestCompile(t *testing.T) {
	tests := []struct {
		name    string
		sched   store.MaintenanceSchedule
		wantErr bool
	}{
		{"once valid", store.MaintenanceSchedule{Kind: store.ScheduleOnce, StartsMs: 1000, EndsMs: 2000}, false},
		{"once start equals end", store.MaintenanceSchedule{Kind: store.ScheduleOnce, StartsMs: 1000, EndsMs: 1000}, true},
		{"once start after end", store.MaintenanceSchedule{Kind: store.ScheduleOnce, StartsMs: 2000, EndsMs: 1000}, true},
		{"once with cron", store.MaintenanceSchedule{Kind: store.ScheduleOnce, StartsMs: 1000, EndsMs: 2000, Cron: "0 3 * * *"}, true},
		{"recurring valid", recurring("0 3 * * *", 60, "Europe/Berlin"), false},
		{"recurring empty timezone is UTC", recurring("0 3 * * *", 60, ""), false},
		{"recurring longest duration", recurring("0 3 * * *", 10080, ""), false},
		{"recurring duration zero", recurring("0 3 * * *", 0, ""), true},
		{"recurring duration above one week", recurring("0 3 * * *", 10081, ""), true},
		{"recurring bad timezone", recurring("0 3 * * *", 60, "Mars/Olympus"), true},
		{"recurring bad cron", recurring("61 * * * *", 60, ""), true},
		{"recurring bounds only start", store.MaintenanceSchedule{Kind: store.ScheduleRecurring, Cron: "0 3 * * *", DurationMinutes: 60, StartsMs: 1000}, false},
		{"recurring bounds reversed", store.MaintenanceSchedule{Kind: store.ScheduleRecurring, Cron: "0 3 * * *", DurationMinutes: 60, StartsMs: 2000, EndsMs: 1000}, true},
		{"unknown kind", store.MaintenanceSchedule{Kind: "weekly", StartsMs: 1000, EndsMs: 2000}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(tc.sched)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Compile() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestOnceOccurrence(t *testing.T) {
	sch := mustCompile(t, store.MaintenanceSchedule{
		Kind:     store.ScheduleOnce,
		StartsMs: mustParseTime(t, "2026-06-01T10:00:00Z").UnixMilli(),
		EndsMs:   mustParseTime(t, "2026-06-01T11:00:00Z").UnixMilli(),
	})
	runOccurrence(t, sch, []occCase{
		{"2026-06-01T09:59:00Z", "", ""},
		{"2026-06-01T10:00:00Z", "2026-06-01T10:00:00Z", "2026-06-01T11:00:00Z"},
		{"2026-06-01T10:30:00Z", "2026-06-01T10:00:00Z", "2026-06-01T11:00:00Z"},
		{"2026-06-01T11:00:00Z", "", ""},
		{"2026-06-01T12:00:00Z", "", ""},
	})
}

func TestOnceNextStart(t *testing.T) {
	sch := mustCompile(t, store.MaintenanceSchedule{
		Kind:     store.ScheduleOnce,
		StartsMs: mustParseTime(t, "2026-06-01T10:00:00Z").UnixMilli(),
		EndsMs:   mustParseTime(t, "2026-06-01T11:00:00Z").UnixMilli(),
	})
	runNext(t, sch, []nextCase{
		{"2026-06-01T09:00:00Z", "2026-06-01T10:00:00Z"},
		{"2026-06-01T10:00:00Z", ""},
		{"2026-06-01T10:30:00Z", ""},
	})
}

func TestRecurringDST(t *testing.T) {
	tests := []struct {
		name  string
		sched store.MaintenanceSchedule
		occ   []occCase
		next  []nextCase
	}{
		{
			// Spring forward 2026-03-29 (01:00 UTC): 01:30 CET is 00:30 UTC, the
			// window spans the change and must still last 60 real minutes.
			name:  "spans spring-forward transition",
			sched: recurring("30 1 * * *", 60, "Europe/Berlin"),
			occ: []occCase{
				{"2026-03-29T00:29:00Z", "", ""},
				{"2026-03-29T00:30:00Z", "2026-03-29T00:30:00Z", "2026-03-29T01:30:00Z"},
				{"2026-03-29T01:00:00Z", "2026-03-29T00:30:00Z", "2026-03-29T01:30:00Z"},
				{"2026-03-29T01:29:00Z", "2026-03-29T00:30:00Z", "2026-03-29T01:30:00Z"},
				{"2026-03-29T01:30:00Z", "", ""},
			},
		},
		{
			// 02:30 does not exist on 2026-03-29; it starts at the jump, 03:00 CEST = 01:00 UTC.
			name:  "nonexistent local time starts at the jump",
			sched: recurring("30 2 * * *", 60, "Europe/Berlin"),
			occ: []occCase{
				{"2026-03-29T00:59:00Z", "", ""},
				{"2026-03-29T01:00:00Z", "2026-03-29T01:00:00Z", "2026-03-29T02:00:00Z"},
				{"2026-03-29T01:59:00Z", "2026-03-29T01:00:00Z", "2026-03-29T02:00:00Z"},
				{"2026-03-29T02:00:00Z", "", ""},
			},
			next: []nextCase{
				{"2026-03-28T12:00:00Z", "2026-03-29T01:00:00Z"},
				{"2026-03-29T01:00:00Z", "2026-03-30T00:30:00Z"},
			},
		},
		{
			// Fall back 2026-10-25: 02:30 CEST (00:30 UTC) and 02:30 CET (01:30 UTC)
			// both exist; only the first starts a window.
			name:  "ambiguous local time starts once",
			sched: recurring("30 2 * * *", 60, "Europe/Berlin"),
			occ: []occCase{
				{"2026-10-25T00:29:00Z", "", ""},
				{"2026-10-25T00:30:00Z", "2026-10-25T00:30:00Z", "2026-10-25T01:30:00Z"},
				{"2026-10-25T01:15:00Z", "2026-10-25T00:30:00Z", "2026-10-25T01:30:00Z"},
				{"2026-10-25T01:30:00Z", "", ""},
				{"2026-10-25T01:45:00Z", "", ""},
				{"2026-10-26T01:30:00Z", "2026-10-26T01:30:00Z", "2026-10-26T02:30:00Z"},
			},
			next: []nextCase{
				{"2026-10-24T12:00:00Z", "2026-10-25T00:30:00Z"},
				{"2026-10-25T00:30:00Z", "2026-10-26T01:30:00Z"},
			},
		},
		{
			// Daily 03:00 on both transition dates: 03:00 CEST = 01:00 UTC, 03:00 CET = 02:00 UTC.
			name:  "daily 03:00 across both transitions",
			sched: recurring("0 3 * * *", 60, "Europe/Berlin"),
			occ: []occCase{
				{"2026-03-29T01:00:00Z", "2026-03-29T01:00:00Z", "2026-03-29T02:00:00Z"},
				{"2026-03-29T02:30:00Z", "", ""},
				{"2026-10-25T02:00:00Z", "2026-10-25T02:00:00Z", "2026-10-25T03:00:00Z"},
				{"2026-10-25T02:30:00Z", "2026-10-25T02:00:00Z", "2026-10-25T03:00:00Z"},
			},
		},
		{
			// 02:00 on 2026-10-25 is ambiguous: window starts at 00:00 UTC and lasts 120 real minutes.
			name:  "window spanning fall-back lasts real minutes",
			sched: recurring("0 2 * * *", 120, "Europe/Berlin"),
			occ: []occCase{
				{"2026-10-25T00:00:00Z", "2026-10-25T00:00:00Z", "2026-10-25T02:00:00Z"},
				{"2026-10-25T01:00:00Z", "2026-10-25T00:00:00Z", "2026-10-25T02:00:00Z"},
				{"2026-10-25T02:00:00Z", "", ""},
			},
		},
		{
			// Weekly Sunday 22:00 for 240 minutes crosses midnight into Monday.
			name:  "weekly crossing midnight",
			sched: recurring("0 22 * * SUN", 240, "Europe/Berlin"),
			occ: []occCase{
				{"2026-10-18T19:59:00Z", "", ""},
				{"2026-10-18T20:00:00Z", "2026-10-18T20:00:00Z", "2026-10-19T00:00:00Z"},
				{"2026-10-18T23:30:00Z", "2026-10-18T20:00:00Z", "2026-10-19T00:00:00Z"},
				{"2026-10-19T00:00:00Z", "", ""},
			},
		},
		{
			// Monthly on the 1st at 01:00 local: CEST before 2026-10-25, CET after.
			name:  "monthly on the first",
			sched: recurring("0 1 1 * *", 60, "Europe/Berlin"),
			occ: []occCase{
				{"2026-09-30T22:59:00Z", "", ""},
				{"2026-09-30T23:00:00Z", "2026-09-30T23:00:00Z", "2026-10-01T00:00:00Z"},
				{"2026-11-01T00:30:00Z", "2026-11-01T00:00:00Z", "2026-11-01T01:00:00Z"},
			},
			next: []nextCase{
				{"2026-09-30T22:30:00Z", "2026-09-30T23:00:00Z"},
				{"2026-10-15T00:00:00Z", "2026-11-01T00:00:00Z"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sch := mustCompile(t, tc.sched)
			runOccurrence(t, sch, tc.occ)
			runNext(t, sch, tc.next)
		})
	}
}

func TestRecurringNextStart(t *testing.T) {
	tests := []struct {
		name  string
		sched store.MaintenanceSchedule
		next  []nextCase
	}{
		{
			name:  "every 30 minutes, UTC",
			sched: recurring("*/30 * * * *", 45, ""),
			next: []nextCase{
				{"2026-06-01T10:40:00Z", "2026-06-01T11:00:00Z"},
				{"2026-06-01T10:30:00Z", "2026-06-01T11:00:00Z"},
			},
		},
		{
			name:  "impossible date returns none",
			sched: recurring("0 0 31 2 *", 60, "Europe/Berlin"),
			next: []nextCase{
				{"2026-01-01T00:00:00Z", ""},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runNext(t, mustCompile(t, tc.sched), tc.next)
		})
	}
}

func TestImpossibleScheduleNeverOccurs(t *testing.T) {
	sch := mustCompile(t, recurring("0 0 31 2 *", 60, "Europe/Berlin"))
	runOccurrence(t, sch, []occCase{
		{"2026-03-01T00:30:00Z", "", ""},
		{"2026-06-01T00:30:00Z", "", ""},
	})
}

func TestRecurringBounds(t *testing.T) {
	bounded := store.MaintenanceSchedule{
		Kind:            store.ScheduleRecurring,
		Cron:            "0 3 * * *",
		DurationMinutes: 60,
		StartsMs:        mustParseTime(t, "2026-06-01T00:00:00Z").UnixMilli(),
		EndsMs:          mustParseTime(t, "2026-06-03T00:00:00Z").UnixMilli(),
	}
	tests := []struct {
		name  string
		sched store.MaintenanceSchedule
		occ   []occCase
		next  []nextCase
	}{
		{
			name:  "starts and ends bound the starts",
			sched: bounded,
			occ: []occCase{
				{"2026-05-31T03:30:00Z", "", ""},
				{"2026-06-01T03:30:00Z", "2026-06-01T03:00:00Z", "2026-06-01T04:00:00Z"},
				{"2026-06-02T03:30:00Z", "2026-06-02T03:00:00Z", "2026-06-02T04:00:00Z"},
				{"2026-06-03T03:30:00Z", "", ""},
			},
			next: []nextCase{
				{"2026-05-31T12:00:00Z", "2026-06-01T03:00:00Z"},
				{"2026-06-02T03:00:00Z", ""},
			},
		},
		{
			name: "end bound does not cut an occurrence in progress",
			sched: store.MaintenanceSchedule{
				Kind:            store.ScheduleRecurring,
				Cron:            "0 3 * * *",
				DurationMinutes: 60,
				EndsMs:          mustParseTime(t, "2026-06-02T03:30:00Z").UnixMilli(),
			},
			occ: []occCase{
				{"2026-06-02T03:45:00Z", "2026-06-02T03:00:00Z", "2026-06-02T04:00:00Z"},
				{"2026-06-03T03:15:00Z", "", ""},
			},
			next: []nextCase{
				{"2026-06-01T03:00:00Z", "2026-06-02T03:00:00Z"},
				{"2026-06-02T03:00:00Z", ""},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sch := mustCompile(t, tc.sched)
			runOccurrence(t, sch, tc.occ)
			runNext(t, sch, tc.next)
		})
	}
}

func TestOverlappingOccurrences(t *testing.T) {
	// Starts every 30 minutes, each lasting 45: the window with the latest end wins.
	sch := mustCompile(t, recurring("*/30 * * * *", 45, ""))
	runOccurrence(t, sch, []occCase{
		{"2026-06-01T10:10:00Z", "2026-06-01T10:00:00Z", "2026-06-01T10:45:00Z"},
		{"2026-06-01T10:40:00Z", "2026-06-01T10:30:00Z", "2026-06-01T11:15:00Z"},
		{"2026-06-01T11:00:00Z", "2026-06-01T11:00:00Z", "2026-06-01T11:45:00Z"},
		{"2026-06-01T11:20:00Z", "2026-06-01T11:00:00Z", "2026-06-01T11:45:00Z"},
		{"2026-06-01T11:50:00Z", "2026-06-01T11:30:00Z", "2026-06-01T12:15:00Z"},
	})
}
