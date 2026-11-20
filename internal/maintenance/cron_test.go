// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package maintenance

import (
	"strings"
	"testing"
	"time"
)

func TestParseCronValid(t *testing.T) {
	tests := []string{
		"* * * * *",
		"0 3 * * *",
		"*/15 * * * *",
		"0-30/10 * * * *",
		"5/20 * * * *",
		"1,15,30 * * * *",
		"0 0 * JAN,jun *",
		"0 0 * * sun-sat",
		"0 0 * * 7",
		"0 0 * * 5-7",
		"0 0 1-31/2 * *",
		"0 0 * * SUN",
		"  0   3  *  *  *  ",
		"@daily",
		"@midnight",
		"@weekly",
		"@monthly",
		"@yearly",
		"@annually",
		"@hourly",
		"@DAILY",
	}
	for _, expr := range tests {
		t.Run(expr, func(t *testing.T) {
			if _, err := ParseCron(expr); err != nil {
				t.Fatalf("ParseCron(%q) error: %v", expr, err)
			}
		})
	}
}

func TestParseCronInvalid(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want string // substring expected in the error message
	}{
		{"empty", "", "5 fields"},
		{"four fields", "* * * *", "5 fields"},
		{"six fields", "* * * * * *", "5 fields"},
		{"minute out of range", "60 * * * *", "minute"},
		{"hour out of range", "* 24 * * *", "hour"},
		{"day-of-month zero", "* * 0 * *", "day-of-month"},
		{"day-of-month too large", "* * 32 * *", "day-of-month"},
		{"month zero", "* * * 0 *", "month"},
		{"month too large", "* * * 13 *", "month"},
		{"weekday too large", "* * * * 8", "day-of-week"},
		{"weekday range too large", "* * * * 0-8", "out of range"},
		{"zero step", "*/0 * * * *", "step"},
		{"non-numeric step", "*/x * * * *", "step"},
		{"step larger than range", "*/61 * * * *", "step"},
		{"malformed range", "1-2-3 * * * *", "minute"},
		{"reversed range", "5-1 * * * *", "reversed"},
		{"reversed weekday names", "* * * * sat-sun", "reversed"},
		{"unknown month name", "* * * FOO *", "month"},
		{"full weekday name", "* * * * MONDAY", "day-of-week"},
		{"empty list element", "1,,2 * * * *", "minute"},
		{"negative value", "-1 * * * *", "minute"},
		{"fractional value", "1.5 * * * *", "minute"},
		{"unknown macro", "@reboot", "macro"},
		{"unsupported character", "* * * * ?", "day-of-week"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCron(tc.expr)
			if err == nil {
				t.Fatalf("ParseCron(%q) succeeded, want error", tc.expr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ParseCron(%q) error = %q, want it to contain %q", tc.expr, err, tc.want)
			}
		})
	}
}

func TestCronMatches(t *testing.T) {
	// June 1 2026 is a Monday, June 7 a Sunday, June 15 a Monday, June 17 a Wednesday.
	tests := []struct {
		name string
		expr string
		at   string
		want bool
	}{
		{"fixed time matches", "0 3 * * *", "2026-06-01T03:00:00Z", true},
		{"minute off", "0 3 * * *", "2026-06-01T03:01:00Z", false},
		{"hour off", "0 3 * * *", "2026-06-01T04:00:00Z", false},
		{"step matches", "*/15 * * * *", "2026-06-01T10:45:00Z", true},
		{"step misses", "*/15 * * * *", "2026-06-01T10:44:00Z", false},
		{"day list", "0 0 1,15 * *", "2026-06-15T00:00:00Z", true},
		{"day list misses", "0 0 1,15 * *", "2026-06-14T00:00:00Z", false},
		{"dom OR dow: dom matches", "0 0 15 * 3", "2026-06-15T00:00:00Z", true},
		{"dom OR dow: dow matches", "0 0 15 * 3", "2026-06-17T00:00:00Z", true},
		{"dom OR dow: neither matches", "0 0 15 * 3", "2026-06-16T00:00:00Z", false},
		{"dom OR dow: monday with dom 1", "0 0 1 * 1", "2026-06-01T00:00:00Z", true},
		{"dom OR dow: monday with dom 8", "0 0 1 * 1", "2026-06-08T00:00:00Z", true},
		{"dom OR dow: tuesday dom 2", "0 0 1 * 1", "2026-06-02T00:00:00Z", false},
		{"dom star ANDs dow", "0 0 * * 1", "2026-06-08T00:00:00Z", true},
		{"dom star ANDs dow misses", "0 0 * * 1", "2026-06-09T00:00:00Z", false},
		{"sunday as 0", "0 0 * * 0", "2026-06-07T00:00:00Z", true},
		{"sunday as 7", "0 0 * * 7", "2026-06-07T00:00:00Z", true},
		{"7 is not monday", "0 0 * * 7", "2026-06-01T00:00:00Z", false},
		{"weekday range with 7", "0 0 * * 5-7", "2026-06-05T00:00:00Z", true},
		{"weekday range saturday", "0 0 * * 5-7", "2026-06-06T00:00:00Z", true},
		{"weekday range monday", "0 0 * * 5-7", "2026-06-08T00:00:00Z", false},
		{"month names case-insensitive", "0 0 * JAN,jun *", "2026-06-01T00:00:00Z", true},
		{"month names miss", "0 0 * JAN,jun *", "2026-02-01T00:00:00Z", false},
		{"weekday name uppercase", "0 0 * * SUN", "2026-06-07T00:00:00Z", true},
		{"macro daily", "@daily", "2026-06-01T00:00:00Z", true},
		{"macro daily misses", "@daily", "2026-06-01T00:01:00Z", false},
		{"macro weekly", "@weekly", "2026-06-07T00:00:00Z", true},
		{"macro monthly", "@monthly", "2026-06-01T00:00:00Z", true},
		{"macro hourly", "@hourly", "2026-06-01T12:00:00Z", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseCron(tc.expr)
			if err != nil {
				t.Fatalf("ParseCron(%q): %v", tc.expr, err)
			}
			at := mustParseTime(t, tc.at)
			if got := c.Matches(at); got != tc.want {
				t.Errorf("Matches(%s) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

func TestCronMatchesUsesLocationOfTime(t *testing.T) {
	berlin := mustLocation(t, "Europe/Berlin")
	c, err := ParseCron("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"03:00 local (CEST) matches", time.Date(2026, 3, 29, 3, 0, 0, 0, berlin), true},
		{"03:00 in UTC matches in UTC", time.Date(2026, 3, 29, 3, 0, 0, 0, time.UTC), true},
		{"01:00 UTC is 03:00 local after conversion", time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC).In(berlin), true},
		{"01:00 UTC does not match in UTC", time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC), false},
		{"03:00 local in winter matches", time.Date(2026, 1, 15, 3, 0, 0, 0, berlin), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Matches(tc.at); got != tc.want {
				t.Errorf("Matches(%s) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load location %q: %v", name, err)
	}
	return loc
}
