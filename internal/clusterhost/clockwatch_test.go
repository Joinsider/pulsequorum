// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package clusterhost

import (
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/degraded"
)

// ADR 0020: the offset is remote minus the local midpoint of the call, and
// samples with a round trip above one second are discarded.
func TestEstimateOffset(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// ADR 0024: the last GetStatus answer per peer is kept as a PeerView, and
// answers older than clockMaxAge are dropped.
func TestPeerViewTrackerFreshness(t *testing.T) {
	t.Parallel()
	base := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	var tr peerViewTracker
	tr.record("b", degraded.PeerView{AnsweredAt: base, LeaderID: "node-c"})
	tr.record("c", degraded.PeerView{AnsweredAt: base.Add(time.Minute), SelfCheckFailed: true})
	tr.record("b", degraded.PeerView{AnsweredAt: base.Add(10 * time.Second), LeaderID: ""})

	got := tr.snapshot(base.Add(90 * time.Second))
	if want := (degraded.PeerView{AnsweredAt: base.Add(10 * time.Second)}); got["b"] != want {
		t.Errorf("view of b = %+v, want the newer answer %+v", got["b"], want)
	}
	if !got["c"].SelfCheckFailed {
		t.Errorf("view of c = %+v, want self-check failed", got["c"])
	}

	got = tr.snapshot(base.Add(2*time.Minute + 11*time.Second))
	if _, ok := got["b"]; ok {
		t.Errorf("stale view of b still returned: %+v", got)
	}
	if _, ok := got["c"]; !ok {
		t.Errorf("fresh view of c dropped: %+v", got)
	}
	if got := tr.snapshot(base.Add(time.Hour)); len(got) != 0 {
		t.Errorf("views after all aged out = %v, want none", got)
	}
}

// ADR 0024: while no leader is known the poll runs at most every 5 s; a
// shorter configured interval (tests) and a leader known keep the normal one.
func TestClockWait(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		normal     time.Duration
		leaderless bool
		want       time.Duration
	}{
		{"leader known, normal interval", 15 * time.Second, false, 15 * time.Second},
		{"leaderless, default interval shortened to 5 s", 15 * time.Second, true, 5 * time.Second},
		{"leaderless, interval already shorter", 200 * time.Millisecond, true, 200 * time.Millisecond},
		{"leaderless, interval exactly 5 s", 5 * time.Second, true, 5 * time.Second},
		{"leader known, short interval", 200 * time.Millisecond, false, 200 * time.Millisecond},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := clockWait(tc.normal, tc.leaderless); got != tc.want {
				t.Errorf("clockWait = %v, want %v", got, tc.want)
			}
		})
	}
}
