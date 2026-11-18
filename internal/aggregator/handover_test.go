// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package aggregator

import (
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/store"
)

// gapScenario replays the handover of ADR 0033 up to the takeover of the gap
// job. The previous leader evaluates round 0 as UP. Then the node loses
// quorum: rounds 1..5 are UP, rounds 6..9 are DOWN, and a follower proposes
// nothing. The gap job takes over late in round 9 and re-evaluates the slots
// before liveStart (ReevaluateGap, internal/degraded/reconcile.go). It returns
// the rig, the since of the UP status, the takeover time and liveStart. The
// rig is not leader yet.
func gapScenario(t *testing.T) (r *rig, upSince, takeover, liveStart int64) {
	t.Helper()
	r = newRig(t, doc(0, 1, false))
	r.a.opts.LeaderGrace = 10 * time.Second

	// The previous leader evaluates round 0 as UP.
	r.deliver(r.slot(0)+100, r.hb(nodeID, 0, 100, store.CheckUp))
	if r.f.monitor().State.Status != store.StatusUp {
		r.tick(r.slot(0) + 10_200) // grace period over: round 0 is evaluated
	}
	if st := r.f.monitor().State; st.Status != store.StatusUp {
		t.Fatalf("state = %s since %d, want UP after round 0", st.Status, st.SinceMs)
	}
	upSince = r.f.monitor().State.SinceMs
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
	takeover = r.slot(9) + 55_000
	liveStart = takeover - 4*intervalMs
	changes, err := r.a.ReevaluateGap(r.f.ctx, r.f.monitor(), store.StatusUp, upSince, liveStart)
	if err != nil {
		t.Fatalf("ReevaluateGap: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("ReevaluateGap changes = %+v, want none: rounds 1..5 are UP and round 6 starts after liveStart", changes)
	}
	return r, upSince, takeover, liveStart
}

// TestGapHandOverLeavesNoRoundUnevaluated checks ADR 0033: the handover between
// the gap job (ReevaluateGap, internal/degraded/reconcile.go) and the live
// rebuild leaves no round unevaluated. Without the handover point, the gap job
// evaluates the slots before liveStart, and the live aggregator rebuilds from
// its later evaluation time, so the slots in between are evaluated by nobody
// and a DOWN that starts there gets a later since. The gap job records its
// window end with HandOver, and the rebuild replays from there.
func TestGapHandOverLeavesNoRoundUnevaluated(t *testing.T) {
	r, _, takeover, liveStart := gapScenario(t)
	r.a.HandOver(cluster.DefaultTenantID, "m1", liveStart)

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

// TestHandOverWithoutPointKeepsLookback documents the unchanged default: without
// a handover point the rebuild starts at the usual lookback, so round 6 stays
// unevaluated and DOWN is recorded since slot 7.
func TestHandOverWithoutPointKeepsLookback(t *testing.T) {
	r, _, takeover, _ := gapScenario(t)

	r.leader = true
	r.tick(takeover)
	r.tick(takeover + 10_000 + 100)

	st := r.f.monitor().State
	if st.Status != store.StatusDown || st.SinceMs != r.slot(7) {
		t.Errorf("state = %s since slot %d, want DOWN since slot 7 (%d) without a handover point",
			st.Status, (st.SinceMs-r.slot(0))/intervalMs, r.slot(7))
	}
}

// TestHandOverSurvivesLeadershipLoss checks that a handover point outlives a
// leadership of the aggregator that ended before its rebuild: the gap job of
// the next leadership may record its points before the aggregator noticed the
// loss, so only ClearHandOvers drops them (ADR 0033).
func TestHandOverSurvivesLeadershipLoss(t *testing.T) {
	r, _, takeover, liveStart := gapScenario(t)
	r.a.HandOver(cluster.DefaultTenantID, "m1", liveStart)

	// First leadership starts and ends before the grace period is over.
	r.leader = true
	r.tick(takeover)
	r.tick(takeover + 5_000)
	r.leader = false
	r.tick(takeover + 6_000)

	// Second leadership: grace over, the rebuild still replays from the point.
	r.leader = true
	r.tick(takeover + 7_000)
	r.tick(takeover + 7_000 + 10_000 + 100)

	st := r.f.monitor().State
	if st.Status != store.StatusDown || st.SinceMs != r.slot(6) {
		t.Errorf("state = %s since slot %d, want DOWN since slot 6 (%d): the handover point must survive the leadership loss",
			st.Status, (st.SinceMs-r.slot(0))/intervalMs, r.slot(6))
	}
}

// TestClearHandOvers checks that ClearHandOvers drops a recorded point, so the
// rebuild uses the usual lookback and DOWN is recorded since slot 7.
func TestClearHandOvers(t *testing.T) {
	r, _, takeover, liveStart := gapScenario(t)
	r.a.HandOver(cluster.DefaultTenantID, "m1", liveStart)
	r.a.ClearHandOvers()

	r.leader = true
	r.tick(takeover)
	r.tick(takeover + 10_000 + 100)

	st := r.f.monitor().State
	if st.Status != store.StatusDown || st.SinceMs != r.slot(7) {
		t.Errorf("state = %s since slot %d, want DOWN since slot 7 (%d) after ClearHandOvers",
			st.Status, (st.SinceMs-r.slot(0))/intervalMs, r.slot(7))
	}
}
