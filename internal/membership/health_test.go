// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package membership

import (
	"maps"
	"testing"
	"time"
)

// healthEvent is one observation, at an offset from the test's base time.
type healthEvent struct {
	kind string // "failed" or "resumed"
	peer string
	at   time.Duration
}

// TestHealthTracker covers the rules of ADR 0017 (FA-SCH-02): the 10 s
// threshold in both directions, the leader grace and the decided changes.
func TestHealthTracker(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	tests := []struct {
		name     string
		leader   bool // false: BecameLeader is never called
		events   []healthEvent
		now      time.Duration
		peers    []string
		registry map[string]bool
		want     map[string]bool
	}{
		{
			name:     "no events: no change during the leader grace",
			leader:   true,
			now:      5 * time.Second,
			peers:    []string{"p1"},
			registry: map[string]bool{"p1": false},
		},
		{
			name:     "no events: healthy after the leader grace",
			leader:   true,
			now:      10 * time.Second,
			peers:    []string{"p1"},
			registry: map[string]bool{"p1": false},
			want:     map[string]bool{"p1": true},
		},
		{
			name:     "no events: registry already healthy, no proposal",
			leader:   true,
			now:      time.Minute,
			peers:    []string{"p1"},
			registry: map[string]bool{"p1": true},
		},
		{
			name:     "failing for less than the threshold: no change",
			leader:   true,
			events:   []healthEvent{{"failed", "p1", 5 * time.Second}},
			now:      12 * time.Second,
			peers:    []string{"p1"},
			registry: map[string]bool{"p1": true},
		},
		{
			name:     "failing for the threshold: unhealthy",
			leader:   true,
			events:   []healthEvent{{"failed", "p1", time.Second}, {"failed", "p1", 3 * time.Second}},
			now:      11 * time.Second,
			peers:    []string{"p1"},
			registry: map[string]bool{"p1": true},
			want:     map[string]bool{"p1": false},
		},
		{
			name:     "failing for the threshold but already unhealthy: no proposal",
			leader:   true,
			events:   []healthEvent{{"failed", "p1", time.Second}},
			now:      20 * time.Second,
			peers:    []string{"p1"},
			registry: map[string]bool{"p1": false},
		},
		{
			name:     "resumed less than the threshold ago: no change",
			leader:   true,
			events:   []healthEvent{{"failed", "p1", time.Second}, {"resumed", "p1", 5 * time.Second}},
			now:      12 * time.Second,
			peers:    []string{"p1"},
			registry: map[string]bool{"p1": false},
		},
		{
			name:     "resumed for the threshold: healthy",
			leader:   true,
			events:   []healthEvent{{"failed", "p1", time.Second}, {"resumed", "p1", 5 * time.Second}},
			now:      15 * time.Second,
			peers:    []string{"p1"},
			registry: map[string]bool{"p1": false},
			want:     map[string]bool{"p1": true},
		},
		{
			name:     "a new failure restarts the failing period",
			leader:   true,
			events:   []healthEvent{{"failed", "p1", time.Second}, {"resumed", "p1", 2 * time.Second}, {"failed", "p1", 3 * time.Second}},
			now:      12 * time.Second,
			peers:    []string{"p1"},
			registry: map[string]bool{"p1": true},
		},
		{
			name:     "peer missing from the registry is ignored",
			leader:   true,
			events:   []healthEvent{{"failed", "p1", time.Second}},
			now:      20 * time.Second,
			peers:    []string{"p1"},
			registry: map[string]bool{},
		},
		{
			name:     "peer that is no voter is ignored",
			leader:   true,
			events:   []healthEvent{{"failed", "p1", time.Second}},
			now:      20 * time.Second,
			peers:    nil,
			registry: map[string]bool{"p1": true},
		},
		{
			name:     "without leadership nothing is decided",
			leader:   false,
			events:   []healthEvent{{"failed", "p1", time.Second}},
			now:      20 * time.Second,
			peers:    []string{"p1"},
			registry: map[string]bool{"p1": true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tr HealthTracker
			if tt.leader {
				tr.BecameLeader(base)
			}
			for _, e := range tt.events {
				at := base.Add(e.at)
				if e.kind == "failed" {
					tr.Failed(e.peer, at)
				} else {
					tr.Resumed(e.peer, at)
				}
			}
			got := tr.Decide(base.Add(tt.now), tt.peers, tt.registry)
			if !maps.Equal(got, tt.want) {
				t.Errorf("Decide() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestHealthTrackerReset checks that a new leadership starts a new grace
// period and forgets the failures of the previous one.
func TestHealthTrackerReset(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	var tr HealthTracker
	tr.BecameLeader(base)
	tr.Failed("p1", base)
	tr.Reset()
	tr.BecameLeader(base.Add(20 * time.Second))

	registry := map[string]bool{"p1": false}
	if got := tr.Decide(base.Add(25*time.Second), []string{"p1"}, registry); len(got) != 0 {
		t.Errorf("during the new grace: Decide() = %v, want none", got)
	}
	if got := tr.Decide(base.Add(30*time.Second), []string{"p1"}, registry); !got["p1"] {
		t.Errorf("after the new grace: Decide() = %v, want p1 healthy", got)
	}
}
