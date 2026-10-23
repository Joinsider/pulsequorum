// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"testing"
	"time"
)

// FA-CLU-05: a follower forwards only while its leader was heard recently.
func TestLeaderContactStale(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	const heartbeat = time.Second

	tests := []struct {
		name string
		last time.Time
		want bool
	}{
		{"never contacted", time.Time{}, true},
		{"just now", now, false},
		{"within twice the heartbeat", now.Add(-2*heartbeat + time.Millisecond), false},
		{"exactly twice the heartbeat", now.Add(-2 * heartbeat), false},
		{"older than twice the heartbeat", now.Add(-2*heartbeat - time.Millisecond), true},
		{"long ago", now.Add(-time.Hour), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := leaderContactStale(tc.last, now, heartbeat); got != tc.want {
				t.Errorf("leaderContactStale() = %v, want %v", got, tc.want)
			}
		})
	}
}
