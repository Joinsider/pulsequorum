// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package aggregator

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/scheduler"
	"github.com/joinsider/uptimecluster/internal/store"
)

// replayK0 is the slot index of slot 0 of the replay tests, far from zero.
const replayK0 = 1_000_000

var (
	replayNodes = []string{"n1", "n2", "n3"}
	replayPhase = scheduler.Phase("m1", time.Minute).Milliseconds()
)

// replayStart returns the start of slot i of monitor "m1" in Unix milliseconds.
func replayStart(i int64) int64 {
	return replayPhase + (replayK0+i)*intervalMs
}

// replayMonitor returns the monitor "m1" with a 60 s interval.
func replayMonitor(policy string, retries, recovery int) store.Monitor {
	return store.Monitor{TenantID: cluster.DefaultTenantID, ID: "m1", IntervalSeconds: 60, TimeoutSeconds: 10,
		ConsensusPolicy: policy, Retries: retries, RecoveryThreshold: recovery}
}

// replayHeartbeats builds the heartbeats of rounds. Each round is one character
// per node n1..n3: U (UP), D (DOWN) or - (no heartbeat). Heartbeats lie 100 ms
// into their slot.
func replayHeartbeats(rounds []string) []store.Heartbeat {
	var hbs []store.Heartbeat
	var seq uint64
	for i, round := range rounds {
		for j, c := range round {
			var st store.CheckStatus
			switch c {
			case 'U':
				st = store.CheckUp
			case 'D':
				st = store.CheckDown
			default:
				continue
			}
			seq++
			hb := store.Heartbeat{NodeID: replayNodes[j], Incarnation: "inc", Seq: seq, TenantID: cluster.DefaultTenantID,
				MonitorID: "m1", TsMs: replayStart(int64(i)) + 100, Status: st}
			if st == store.CheckDown {
				hb.Message, hb.ErrorClass = "connection refused", "connect"
			}
			hbs = append(hbs, hb)
		}
	}
	return hbs
}

// slotLabel renders a change as "STATUS@slot", the slot relative to slot 0.
func slotLabel(status store.MonitorStatus, sinceMs int64) string {
	return fmt.Sprintf("%s@%d", status, (sinceMs-replayPhase)/intervalMs-replayK0)
}

// describeReplay renders the changes of Reevaluate as slotLabel strings.
func describeReplay(changes []Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, slotLabel(c.Status, c.SinceMs))
	}
	return out
}

// replayCase is one row of TestReevaluate.
type replayCase struct {
	name              string
	policy            string
	from              store.MonitorStatus
	retries, recovery int
	slots             []string // one round per slot, see replayHeartbeats
	fromOff           int64    // ms after the start of slot 0 at which the replay starts
	toSlot            int      // slots before toSlot are replayed; 0 means all
	maint             []int    // slots in a maintenance window
	want              []string // "STATUS@slot"
}

// TestReevaluate covers the rules of the live evaluation on gaps (FA-UPT-07,
// ADR 0024). Slot indexes in want are relative to the first slot of the case.
func TestReevaluate(t *testing.T) {
	tests := []replayCase{
		{name: "majority of two reporters DOWN is DOWN at once", policy: "majority", from: store.StatusUp, retries: 0, recovery: 1,
			slots: []string{"DD-"}, want: []string{"DOWN@0"}},
		{name: "majority with one DOWN of three is PARTIAL", policy: "majority", from: store.StatusUp, retries: 0, recovery: 1,
			slots: []string{"DUU"}, want: []string{"PARTIAL@0"}},
		{name: "any: one DOWN is DOWN", policy: "any", from: store.StatusUp, retries: 0, recovery: 1,
			slots: []string{"DUU"}, want: []string{"DOWN@0"}},
		{name: "all: one DOWN of three is PARTIAL", policy: "all", from: store.StatusUp, retries: 0, recovery: 1,
			slots: []string{"DDU"}, want: []string{"PARTIAL@0"}},
		{name: "all: three DOWN is DOWN", policy: "all", from: store.StatusUp, retries: 0, recovery: 1,
			slots: []string{"DDD"}, want: []string{"DOWN@0"}},
		{name: "retries: PENDING, then DOWN dated to the first failing slot", policy: "majority", from: store.StatusUp, retries: 1, recovery: 1,
			slots: []string{"DDD", "DDD", "DDD"}, want: []string{"PENDING@0", "DOWN@0"}},
		{name: "retries: success during the retries returns to UP", policy: "majority", from: store.StatusUp, retries: 2, recovery: 1,
			slots: []string{"DDD", "DDD", "UUU"}, want: []string{"PENDING@0", "UP@2"}},
		{name: "recovery needs consecutive positive slots, dated to the first", policy: "majority", from: store.StatusDown, retries: 0, recovery: 3,
			slots: []string{"UUU", "UUU", "UUU"}, want: []string{"UP@0"}},
		{name: "recovery streak is reset by a failure", policy: "majority", from: store.StatusDown, retries: 0, recovery: 2,
			slots: []string{"UUU", "DDD", "UUU", "UUU"}, want: []string{"UP@2"}},
		{name: "DOWN episode fully inside the gap", policy: "majority", from: store.StatusUp, retries: 0, recovery: 1,
			slots: []string{"DDD", "UUU"}, want: []string{"DOWN@0", "UP@1"}},
		{name: "DOWN without reports stays DOWN", policy: "majority", from: store.StatusDown, retries: 0, recovery: 1,
			slots: []string{"---", "---"}, want: nil},
		{name: "PENDING without reports stays PENDING", policy: "majority", from: store.StatusPending, retries: 1, recovery: 1,
			slots: []string{"---"}, want: nil},
		{name: "no reports from UP is UNKNOWN", policy: "majority", from: store.StatusUp, retries: 0, recovery: 1,
			slots: []string{"---"}, want: []string{"UNKNOWN@0"}},
		{name: "reports after UNKNOWN return to UP", policy: "majority", from: store.StatusUp, retries: 0, recovery: 1,
			slots: []string{"---", "UUU"}, want: []string{"UNKNOWN@0", "UP@1"}},
		{name: "PAUSED is never left", policy: "majority", from: store.StatusPaused, retries: 0, recovery: 1,
			slots: []string{"DDD"}, want: nil},
		{name: "MAINTENANCE is never left", policy: "majority", from: store.StatusMaintenance, retries: 0, recovery: 1,
			slots: []string{"DDD"}, want: nil},
		{name: "maintenance slot resets the failure streak and is skipped", policy: "majority", from: store.StatusUp, retries: 1, recovery: 1,
			slots: []string{"DDD", "DDD", "DDD"}, maint: []int{1}, want: []string{"PENDING@0"}},
		{name: "maintenance slot resets the recovery streak", policy: "majority", from: store.StatusDown, retries: 0, recovery: 2,
			slots: []string{"UUU", "UUU", "UUU"}, maint: []int{1}, want: nil},
		{name: "a slot starting before fromMs is excluded", policy: "majority", from: store.StatusUp, retries: 0, recovery: 1,
			slots: []string{"DDD"}, fromOff: 1, want: nil},
		{name: "a slot starting at toMs is excluded", policy: "majority", from: store.StatusUp, retries: 0, recovery: 1,
			slots: []string{"UUU", "DDD"}, toSlot: 1, want: nil},
		{name: "the slot before toMs is included", policy: "majority", from: store.StatusUp, retries: 0, recovery: 1,
			slots: []string{"UUU", "DDD"}, want: []string{"DOWN@1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			toSlot := tt.toSlot
			if toSlot == 0 {
				toSlot = len(tt.slots)
			}
			var maintenance func(time.Time) bool
			if len(tt.maint) > 0 {
				var windows []int64
				for _, i := range tt.maint {
					windows = append(windows, replayStart(int64(i)))
				}
				maintenance = func(at time.Time) bool { return slices.Contains(windows, at.UnixMilli()) }
			}
			m := replayMonitor(tt.policy, tt.retries, tt.recovery)
			changes := Reevaluate(m, tt.from, replayStart(0)+tt.fromOff, replayStart(int64(toSlot)),
				replayHeartbeats(tt.slots), replayNodes, maintenance, time.Second)
			for _, c := range changes {
				if (c.Status == store.StatusDown) != (c.Incident != nil) {
					t.Fatalf("change %+v: incident must be set exactly for DOWN", c)
				}
			}
			if got := describeReplay(changes); !slices.Equal(got, tt.want) {
				t.Fatalf("changes = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestReevaluateReportersAndIncident checks the reporters of a change and the
// incident of a DOWN change, whose ID is stable for the same input.
func TestReevaluateReportersAndIncident(t *testing.T) {
	m := replayMonitor("majority", 0, 1)
	hbs := replayHeartbeats([]string{"DD-", "UUU"})
	run := func() []Change {
		return Reevaluate(m, store.StatusUp, replayStart(0), replayStart(2), hbs, replayNodes, nil, time.Second)
	}
	first := run()
	if len(first) != 2 {
		t.Fatalf("changes = %+v, want DOWN and UP", first)
	}
	down := first[0]
	if !equalStrings(down.Reporters, []string{"n1", "n2"}) {
		t.Fatalf("reporters = %v, want [n1 n2]", down.Reporters)
	}
	inc := down.Incident
	if inc == nil || inc.ID == "" || !equalStrings(inc.AffectedNodes, []string{"n1", "n2"}) ||
		inc.Cause != "connection refused" || inc.ErrorClass != "connect" {
		t.Fatalf("incident = %+v", inc)
	}
	if up := first[1]; len(up.Reporters) != 3 || up.Incident != nil {
		t.Fatalf("UP change = %+v", up)
	}
	if again := run(); again[0].Incident.ID != inc.ID {
		t.Fatalf("incident ID changed from %s to %s on the same input", inc.ID, again[0].Incident.ID)
	}
}

// TestReevaluateMatchesLive drives the live aggregator through a sequence of
// rounds with a fake clock and checks that Reevaluate finds the same changes
// for the same heartbeats (ADR 0024, FA-UPT-07).
func TestReevaluateMatchesLive(t *testing.T) {
	rounds := []string{"DDD", "DDD", "UUU", "U--", "DDD", "UUU", "UUU", "DU-", "---", "UUU", "DDD", "D--", "UUU", "UUU"}
	r := newRig(t, doc(1, 2, false))
	r.registry = r.nodes(replayNodes...)
	from := r.f.monitor().State.Status

	var all []store.Heartbeat
	for i, round := range rounds {
		var batch []store.Heartbeat
		for j, c := range round {
			var st store.CheckStatus
			switch c {
			case 'U':
				st = store.CheckUp
			case 'D':
				st = store.CheckDown
			default:
				continue
			}
			batch = append(batch, r.hb(replayNodes[j], int64(i), 100, st))
		}
		all = append(all, batch...)
		if len(batch) > 0 {
			r.deliver(r.slot(int64(i))+100, batch...)
		} else {
			r.tick(r.slot(int64(i)) + 100)
		}
		r.tick(r.slot(int64(i)) + 15000)
	}

	var live []string
	for _, cmd := range r.rec.cmds {
		set := cmd.GetSetMonitorState()
		live = append(live, fmt.Sprintf("%s reporters=%s incident=%t",
			slotLabel(store.MonitorStatus(set.GetStatus()), set.GetSinceMs()),
			strings.Join(set.GetReporters(), ","), set.GetIncident() != nil))
	}
	if len(live) < 5 {
		t.Fatalf("live evaluation proposed %d changes, the sequence is too small to compare", len(live))
	}

	var replay []string
	for _, c := range Reevaluate(r.f.monitor(), from, r.slot(0), r.slot(int64(len(rounds))), all, replayNodes, nil, time.Second) {
		replay = append(replay, fmt.Sprintf("%s reporters=%s incident=%t",
			slotLabel(c.Status, c.SinceMs), strings.Join(c.Reporters, ","), c.Incident != nil))
	}
	if !slices.Equal(replay, live) {
		t.Fatalf("replay = %v\nlive   = %v", replay, live)
	}
}

// TestReevaluateGapLoadsHeartbeats checks that ReevaluateGap reads the stored
// heartbeats and replays them like Reevaluate.
func TestReevaluateGapLoadsHeartbeats(t *testing.T) {
	r := newRig(t, doc(0, 1, false))
	r.registry = r.nodes(replayNodes...)
	var hbs []store.Heartbeat
	hbs = append(hbs, r.hb("n1", 0, 100, store.CheckUp), r.hb("n2", 0, 100, store.CheckUp), r.hb("n3", 0, 100, store.CheckUp))
	hbs = append(hbs, r.hb("n1", 1, 100, store.CheckDown), r.hb("n2", 1, 100, store.CheckDown), r.hb("n3", 1, 100, store.CheckDown))
	hbs = append(hbs, r.hb("n1", 2, 100, store.CheckUp), r.hb("n2", 2, 100, store.CheckUp), r.hb("n3", 2, 100, store.CheckUp))
	if _, err := r.f.metrics.InsertHeartbeats(r.f.ctx, hbs); err != nil {
		t.Fatal(err)
	}

	got, err := r.a.ReevaluateGap(r.f.ctx, r.f.monitor(), store.StatusUp, r.slot(0), r.slot(3))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Status != store.StatusDown || got[0].SinceMs != r.slot(1) ||
		got[1].Status != store.StatusUp || got[1].SinceMs != r.slot(2) {
		t.Fatalf("changes = %+v, want DOWN at slot 1 and UP at slot 2", got)
	}
	if got[0].Incident == nil || got[0].Incident.ID == "" {
		t.Fatalf("DOWN change without incident: %+v", got[0])
	}
}
