// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package aggregator

import (
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/store"
)

// ADR 0022: a slot counts for a node only if it reported and a peer reported
// UP or DEGRADED in the same slot.
func TestDeviationCountsSlotsAgainstPeers(t *testing.T) {
	r := newRig(t, doc(0, 1, false))
	r.registry = r.nodes("n1", "n2", "n3")
	for i := range int64(4) {
		r.deliver(r.slot(i)+100, r.hb("n1", i, 100, store.CheckDown), r.hb("n2", i, 100, store.CheckUp), r.hb("n3", i, 100, store.CheckUp))
	}
	want := map[string]DeviationStats{"n1": {Slots: 4, Down: 4}, "n2": {Slots: 4}, "n3": {Slots: 4}}
	if got := r.a.Deviation(0); !sameStats(got, want) {
		t.Errorf("Deviation = %v, want %v", got, want)
	}
}

// ADR 0022: a slot without any UP or DEGRADED peer is not compared; a node
// that is excluded from the consensus keeps being compared, so that its return
// can be seen.
func TestDeviationSkipsUncomparedSlotsAndKeepsExcludedNodes(t *testing.T) {
	r := newRig(t, doc(0, 1, false))
	r.registry = r.nodes("n1", "n2", "n3")
	r.a.opts.Exclude = func(id string) bool { return id == "n1" }
	// Slot 0: n1 UP, the others DOWN: n1 has no peer to compare with, the
	// others are compared with n1.
	r.deliver(r.slot(0)+100, r.hb("n1", 0, 100, store.CheckUp), r.hb("n2", 0, 100, store.CheckDown), r.hb("n3", 0, 100, store.CheckDown))
	// Slot 1: n1 DOWN, n2 and n3 UP.
	r.deliver(r.slot(1)+100, r.hb("n1", 1, 100, store.CheckDown), r.hb("n2", 1, 100, store.CheckUp), r.hb("n3", 1, 100, store.CheckUp))
	want := map[string]DeviationStats{"n1": {Slots: 1, Down: 1}, "n2": {Slots: 2, Down: 1}, "n3": {Slots: 2, Down: 1}}
	if got := r.a.Deviation(0); !sameStats(got, want) {
		t.Errorf("Deviation = %v, want %v", got, want)
	}
}

// A slot that is evaluated twice (a superseded status change replays it) is
// counted once. Slots older than the window are dropped.
func TestDeviationCountsEachSlotOnceAndExpires(t *testing.T) {
	r := newRig(t, doc(0, 1, false))
	r.registry = r.nodes("n1", "n2")
	w := &watch{m: store.Monitor{TenantID: cluster.DefaultTenantID, ID: "m1"}}
	// n2 is UP next to a DOWN n1: only n1 has a peer to compare with.
	reports := []store.Heartbeat{
		{NodeID: "n1", Status: store.CheckDown},
		{NodeID: "n2", Status: store.CheckUp},
	}
	old := r.slot(-3) // about two minutes before the fake clock
	r.a.mu.Lock()
	r.a.recordDeviation(w, -3, reports, old)
	r.a.recordDeviation(w, -3, reports, old)
	r.a.mu.Unlock()
	want := map[string]DeviationStats{"n1": {Slots: 1, Down: 1}}
	if got := r.a.Deviation(0); !sameStats(got, want) {
		t.Fatalf("Deviation after a replay = %v, want %v", got, want)
	}

	// Six minutes after the old slot, a new slot is recorded: the old one is
	// outside the five-minute window and is dropped.
	r.now = base.Add(6 * time.Minute)
	r.a.mu.Lock()
	r.a.recordDeviation(w, 2, reports, r.slot(2))
	r.a.mu.Unlock()
	if got := r.a.Deviation(0); !sameStats(got, want) {
		t.Errorf("Deviation after expiry = %v, want %v", got, want)
	}
	if len(r.a.deviations) != 1 || len(r.a.deviationSeen) != 1 {
		t.Errorf("kept %d events and %d keys, want 1 each", len(r.a.deviations), len(r.a.deviationSeen))
	}
}

func sameStats(a, b map[string]DeviationStats) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range b {
		if a[k] != v {
			return false
		}
	}
	return true
}
