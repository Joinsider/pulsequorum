// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package uptime

import (
	"math"
	"testing"
	"time"
)

// FA-REP-03: rollups and compaction wait for the replicated heartbeats of
// other voters, but a stalled voter holds them back at most one hour.
func TestHoldBackLimit(t *testing.T) {
	now := t0.Add(10*time.Hour + 30*time.Minute) // 10:30
	tests := []struct {
		name            string
		replicatedUntil time.Time
		want            time.Time
	}{
		{"fully replicated", now, t0.Add(10 * time.Hour)},                                   // 10:30 - grace, floored to 10:00
		{"replication lags 20 minutes", now.Add(-20 * time.Minute), t0.Add(10 * time.Hour)}, // 10:10 - grace, floored to 10:00
		{"replication lags 40 minutes", now.Add(-40 * time.Minute), t0.Add(9 * time.Hour)},  // held at 9:48 -> 9:00
		{"never caught up", time.Time{}, t0.Add(9 * time.Hour)},                             // bounded by now - 1h
		{"replication lags two hours", now.Add(-2 * time.Hour), t0.Add(9 * time.Hour)},      // bounded by now - 1h
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := holdBackLimit(now, tt.replicatedUntil)
			if got != tt.want.UnixMilli() {
				t.Errorf("limit = %s, want %s", time.UnixMilli(got).UTC(), tt.want)
			}
		})
	}
}

func TestReplicationLimit(t *testing.T) {
	now := t0.Add(10 * time.Hour)
	t.Run("without hold-back", func(t *testing.T) {
		j := NewJob(JobOptions{})
		if got := j.replicationLimit(now); got != math.MaxInt64 {
			t.Errorf("limit = %d, want no limit", got)
		}
	})
	t.Run("with hold-back", func(t *testing.T) {
		j := NewJob(JobOptions{ReplicatedUntil: func() time.Time { return now.Add(-5 * time.Minute) }})
		if got, want := j.replicationLimit(now), holdBackLimit(now, now.Add(-5*time.Minute)); got != want {
			t.Errorf("limit = %d, want %d", got, want)
		}
	})
}
