// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package clusterhost

import (
	"testing"
	"time"
)

// ADR 0020: the offset is remote minus the local midpoint of the call, and
// samples with a round trip above one second are discarded.
func TestEstimateOffset(t *testing.T) {
	local := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		rtt    time.Duration
		skew   time.Duration // remote clock minus local clock
		want   time.Duration
		wantOK bool
	}{
		{"same clock", 10 * time.Millisecond, 0, 0, true},
		{"remote ahead 90 s", 200 * time.Millisecond, 90 * time.Second, 90 * time.Second, true},
		{"remote behind 90 s", 200 * time.Millisecond, -90 * time.Second, -90 * time.Second, true},
		{"round trip of exactly one second", time.Second, 2 * time.Second, 2 * time.Second, true},
		{"round trip above one second", time.Second + time.Millisecond, 2 * time.Second, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The remote reads its clock halfway through the call.
			remote := local.Add(tc.rtt/2 + tc.skew)
			got, ok := estimateOffset(local, local.Add(tc.rtt), remote)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("offset = %v, want %v", got, tc.want)
			}
		})
	}
	if _, ok := estimateOffset(local, local.Add(-time.Millisecond), local); ok {
		t.Error("a clock that went backwards during the call must be rejected")
	}
}

// ADR 0020: the last good sample per peer is kept, a failed or slow call does
// not replace it, and a sample older than two minutes is dropped.
func TestClockTrackerFreshness(t *testing.T) {
	base := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	var tr clockTracker
	tr.record("b", clockSample{offset: 90 * time.Second, at: base})
	tr.record("c", clockSample{offset: -90 * time.Second, at: base.Add(time.Minute)})

	got := tr.offsets(base.Add(90 * time.Second))
	if got["b"] != 90_000 || got["c"] != -90_000 {
		t.Errorf("offsets = %v, want b=+90000 c=-90000", got)
	}
	// b is older than two minutes at this time and disappears; c stays.
	got = tr.offsets(base.Add(2*time.Minute + time.Second))
	if _, ok := got["b"]; ok {
		t.Errorf("stale sample of b still returned: %v", got)
	}
	if got["c"] != -90_000 {
		t.Errorf("fresh sample of c = %v, want -90000", got)
	}
	if got := tr.offsets(base.Add(10 * time.Minute)); len(got) != 0 {
		t.Errorf("offsets after all samples aged out = %v, want none", got)
	}

	// A newer sample replaces the old one.
	tr.record("b", clockSample{offset: 3 * time.Second, at: base.Add(time.Hour)})
	if got := tr.offsets(base.Add(time.Hour)); got["b"] != 3000 {
		t.Errorf("replaced sample = %v, want 3000", got)
	}
}
