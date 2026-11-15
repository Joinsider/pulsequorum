// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package harness

import (
	"testing"
	"time"
)

func TestMaxGap(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	tests := []struct {
		name string
		ts   []time.Time
		from time.Time
		to   time.Time
		want time.Duration
	}{
		{
			name: "regular 20 s heartbeats inside window",
			ts:   []time.Time{at(0), at(20), at(40), at(60), at(80)},
			from: at(10), to: at(70),
			want: 20 * time.Second,
		},
		{
			name: "unordered input is sorted",
			ts:   []time.Time{at(40), at(0), at(20)},
			from: at(0), to: at(40),
			want: 20 * time.Second,
		},
		{
			name: "outage inside window dominates",
			ts:   []time.Time{at(0), at(20), at(100), at(120)},
			from: at(0), to: at(120),
			want: 80 * time.Second,
		},
		{
			name: "anchors before and after the window",
			ts:   []time.Time{at(-15), at(35)},
			from: at(0), to: at(20),
			// prev -15 covers to 35 (50 s) because nothing lies in (0,20].
			want: 50 * time.Second,
		},
		{
			name: "no heartbeats at all yields the window",
			ts:   nil,
			from: at(0), to: at(60),
			want: 60 * time.Second,
		},
		{
			name: "gap to the window end when the last beat is early",
			ts:   []time.Time{at(0), at(10)},
			from: at(0), to: at(50),
			want: 40 * time.Second,
		},
		{
			name: "gap from the window start to the first beat",
			ts:   []time.Time{at(30), at(50)},
			from: at(0), to: at(50),
			want: 30 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MaxGap(tt.ts, tt.from, tt.to)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("MaxGap = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestMaxGapRejectsEmptyWindow(t *testing.T) {
	now := time.Now()
	if _, err := MaxGap(nil, now, now); err == nil {
		t.Error("empty window accepted")
	}
}
