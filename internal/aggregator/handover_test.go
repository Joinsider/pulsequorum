// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package aggregator

import (
	"os"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/store"
)

// TestGapHandOverLeavesNoRoundUnevaluated reproduces ADR 0033: the handover
// between the gap job (ReevaluateGap, internal/degraded/reconcile.go) and the
// live rebuild leaves rounds unevaluated. The gap job evaluates the slots that
// start before liveStart; the live aggregator rebuilds from its own later
// evaluation time, so the slots in between are evaluated by nobody and a DOWN
// that starts there gets a later since. The test is skipped until ADR 0033 is
// implemented; run it with UC_REPRO_ADR0033=1 to see the failure.
func TestGapHandOverLeavesNoRoundUnevaluated(t *testing.T) {
	if os.Getenv("UC_REPRO_ADR0033") == "" {
		t.Skip("reproduces the handover gap of ADR 0033; set UC_REPRO_ADR0033=1 to run it")
	}
	r := newRig(t, doc(0, 1, false))
	r.a.opts.LeaderGrace = 10 * time.Second

	// The previous leader evaluates round 0 as UP.
	r.deliver(r.slot(0)+100, r.hb(nodeID, 0, 100, store.CheckUp))
	if r.f.monitor().State.Status != store.StatusUp {
		r.tick(r.slot(0) + 10_200) // grace period over: round 0 is evaluated
	}
	if st := r.f.monitor().State; st.Status != store.StatusUp {
		t.Fatalf("state = %s since %d, want UP after round 0", st.Status, st.SinceMs)
	}
	upSince := r.f.monitor().State.SinceMs
	n := len(r.rec.cmds)

	// Quorum lost: the leader drops leadership. Rounds 1..5 are UP, rounds
	// 6..9 are DOWN; a follower proposes nothing.
	r.leader = false
	r.tick(r.slot(0) + 10_300)
	for i := int64(1); i <= 5; i++ {
		r.deliver(r.slot(i)+100, r.hb(nodeID, i, 100, store.CheckUp))
	}
	for i := int64(6); i <= 9; i++ {
		r.deliver(r.slot(i)+100, r.hb(nodeID, i, 100, store.CheckDown))
	}
	r.wantCommands(n)

	// The gap job takes over late in round 9. liveStart is (lookback+1) = 4
	// intervals before the takeover, the formula of GapJob.reevaluate in
	// internal/degraded/reconcile.go: the gap job evaluates the slots that
	// start before it, the live aggregator the rest.
	takeover := r.slot(9) + 55_000
	liveStart := takeover - 4*intervalMs
	changes, err := r.a.ReevaluateGap(r.f.ctx, r.f.monitor(), store.StatusUp, upSince, liveStart)
	if err != nil {
		t.Fatalf("ReevaluateGap: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("ReevaluateGap changes = %+v, want none: rounds 1..5 are UP and round 6 starts after liveStart", changes)
	}

	// The live aggregator becomes leader after the gap job: leadership starts,
	// then the grace period is over and it rebuilds and replays.
	r.leader = true
	r.tick(takeover)
	r.tick(takeover + 10_000 + 100)

	// The correct behaviour: DOWN since the first DOWN round, slot 6.
	st := r.f.monitor().State
	if st.Status != store.StatusDown || st.SinceMs != r.slot(6) {
		t.Errorf("state = %s since %d (slot %d), want DOWN since slot 6 (%d): rounds in [liveStart, rebuild floor) are evaluated by nobody",
			st.Status, st.SinceMs, (st.SinceMs-r.slot(0))/intervalMs, r.slot(6))
	}
}
