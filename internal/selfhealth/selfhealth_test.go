// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package selfhealth

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/aggregator"
	"github.com/joinsider/pulsequorum/internal/cluster"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/selfcheck"
	"github.com/joinsider/pulsequorum/internal/store"
)

// selfID is the node this assessor runs on in every test.
const selfID = "n1"

var base = time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)

type fakeRegistry struct{ nodes []store.Node }

func (f *fakeRegistry) ListNodes(context.Context) ([]store.Node, error) {
	return slices.Clone(f.nodes), nil
}

// fakeCluster records the proposals and answers them with err.
type fakeCluster struct {
	cmds []*pb.Command
	err  error
}

func (f *fakeCluster) Apply(_ context.Context, cmd *pb.Command) (cluster.ApplyResult, error) {
	f.cmds = append(f.cmds, cmd)
	return cluster.ApplyResult{}, f.err
}

// rig drives an Assessor with a fake clock. The reference state of a node is
// set per test: refFailed has an entry for every node with configured targets
// (true: all failed), refAge is the age of its round.
type rig struct {
	t         *testing.T
	a         *Assessor
	now       time.Time
	leader    bool
	reg       *fakeRegistry
	app       *fakeCluster
	refFailed map[string]bool
	refAge    map[string]time.Duration
	dev       map[string]aggregator.DeviationStats
}

func newRig(t *testing.T, nodes ...store.Node) *rig {
	t.Helper()
	r := &rig{t: t, now: base, leader: true, reg: &fakeRegistry{nodes: nodes}, app: &fakeCluster{},
		refFailed: map[string]bool{}, refAge: map[string]time.Duration{}, dev: map[string]aggregator.DeviationStats{}}
	r.a = New(Options{
		NodeID:   selfID,
		IsLeader: func() bool { return r.leader },
		Registry: r.reg,
		Cluster:  r.app,
		Self:     func() selfcheck.Status { return r.status(selfID) },
		Peers: func() map[string]selfcheck.Remote {
			out := map[string]selfcheck.Remote{}
			for id := range r.refFailed {
				if id != selfID {
					out[id] = selfcheck.Remote{Status: r.status(id), ReceivedAt: r.now, Age: r.refAge[id]}
				}
			}
			return out
		},
		Deviation: func(int64) map[string]aggregator.DeviationStats { return r.dev },
		FailRatio: 0.5,
		Now:       func() time.Time { return r.now },
		Logger:    slog.New(slog.DiscardHandler),
	})
	return r
}

func (r *rig) status(id string) selfcheck.Status {
	failed, ok := r.refFailed[id]
	if !ok {
		return selfcheck.Status{}
	}
	return selfcheck.Status{Configured: true, Failed: failed, AtMs: r.now.Add(-r.refAge[id]).UnixMilli()}
}

// tick runs one assessment at the current time.
func (r *rig) tick() { r.a.tick(context.Background()) }

// step advances by one assessment interval and runs the assessment.
func (r *rig) step() {
	r.now = r.now.Add(DefaultInterval)
	r.tick()
}

// impairedProposals returns the node → reason of the impaired proposals.
func (r *rig) impairedProposals() map[string]string {
	out := map[string]string{}
	for _, cmd := range r.app.cmds {
		p := cmd.GetSetNodeConnectivity()
		if p.GetImpaired() {
			out[p.GetNodeId()] = p.GetReason()
		}
	}
	return out
}

func voters(ids ...string) []store.Node {
	out := make([]store.Node, 0, len(ids))
	for _, id := range ids {
		out = append(out, store.Node{ID: id, Role: store.NodeRoleVoter, Healthy: true})
	}
	return out
}

// TestDecisions runs the assessor for a number of assessments and compares the
// impaired nodes with the expected reasons (ADR 0022).
func TestDecisions(t *testing.T) {
	tests := []struct {
		name   string
		nodes  []string
		ref    map[string]bool
		refAge map[string]time.Duration
		dev    map[string]aggregator.DeviationStats
		rounds int
		want   map[string]string
	}{
		{
			name:   "reference failed on one of three is impaired after two rounds",
			nodes:  []string{"n1", "n2", "n3"},
			ref:    map[string]bool{"n1": true, "n2": false, "n3": false},
			rounds: 2,
			want:   map[string]string{"n1": reasonReference},
		},
		{
			name:   "one round with a signal is not enough",
			nodes:  []string{"n1", "n2", "n3"},
			ref:    map[string]bool{"n1": true, "n2": false, "n3": false},
			rounds: 1,
			want:   map[string]string{},
		},
		{
			name:   "all targets failing everywhere is no exclusion",
			nodes:  []string{"n1", "n2", "n3"},
			ref:    map[string]bool{"n1": true, "n2": true, "n3": true},
			rounds: 5,
			want:   map[string]string{},
		},
		{
			name:   "two of three signalling is no exclusion",
			nodes:  []string{"n1", "n2", "n3"},
			ref:    map[string]bool{"n1": true, "n2": true, "n3": false},
			rounds: 3,
			want:   map[string]string{},
		},
		{
			name:   "one of five signalling is excluded",
			nodes:  []string{"n1", "n2", "n3", "n4", "n5"},
			ref:    map[string]bool{"n1": true, "n2": false, "n3": false, "n4": false, "n5": false},
			rounds: 2,
			want:   map[string]string{"n1": reasonReference},
		},
		{
			name:   "four of five signalling is no exclusion",
			nodes:  []string{"n1", "n2", "n3", "n4", "n5"},
			ref:    map[string]bool{"n1": true, "n2": true, "n3": true, "n4": true, "n5": false},
			rounds: 3,
			want:   map[string]string{},
		},
		{
			name:   "three of five signalling is no exclusion",
			nodes:  []string{"n1", "n2", "n3", "n4", "n5"},
			ref:    map[string]bool{"n1": true, "n2": true, "n3": true, "n4": false, "n5": false},
			rounds: 3,
			want:   map[string]string{},
		},
		{
			name:   "two of five signalling are both excluded",
			nodes:  []string{"n1", "n2", "n3", "n4", "n5"},
			ref:    map[string]bool{"n1": true, "n2": true, "n3": false, "n4": false, "n5": false},
			rounds: 2,
			want:   map[string]string{"n1": reasonReference, "n2": reasonReference},
		},
		{
			name:   "two voters: the other voter is healthy, so the signalling one is excluded",
			nodes:  []string{"n1", "n2"},
			ref:    map[string]bool{"n1": true, "n2": false},
			rounds: 2,
			want:   map[string]string{"n1": reasonReference},
		},
		{
			name:   "a single voter is never excluded",
			nodes:  []string{"n1"},
			ref:    map[string]bool{"n1": true},
			rounds: 5,
			want:   map[string]string{},
		},
		{
			name:   "a round older than 90 s is no reference signal",
			nodes:  []string{"n1", "n2", "n3"},
			ref:    map[string]bool{"n1": true, "n2": false, "n3": false},
			refAge: map[string]time.Duration{"n1": 100 * time.Second},
			rounds: 3,
			want:   map[string]string{},
		},
		{
			name:   "a node without configured targets gives no reference signal",
			nodes:  []string{"n1", "n2", "n3"},
			ref:    map[string]bool{"n2": false, "n3": false},
			rounds: 3,
			want:   map[string]string{},
		},
		{
			name:   "deviation with ten slots at the ratio is impaired",
			nodes:  []string{"n1", "n2", "n3"},
			dev:    map[string]aggregator.DeviationStats{"n2": {Slots: 10, Down: 5}},
			rounds: 2,
			want:   map[string]string{"n2": reasonDeviation},
		},
		{
			name:   "deviation with nine slots is no signal",
			nodes:  []string{"n1", "n2", "n3"},
			dev:    map[string]aggregator.DeviationStats{"n2": {Slots: 9, Down: 9}},
			rounds: 3,
			want:   map[string]string{},
		},
		{
			name:   "deviation below the ratio is no signal",
			nodes:  []string{"n1", "n2", "n3"},
			dev:    map[string]aggregator.DeviationStats{"n2": {Slots: 20, Down: 9}},
			rounds: 3,
			want:   map[string]string{},
		},
		{
			name:   "reference wins over deviation as reason",
			nodes:  []string{"n1", "n2", "n3"},
			ref:    map[string]bool{"n2": true, "n1": false, "n3": false},
			dev:    map[string]aggregator.DeviationStats{"n2": {Slots: 10, Down: 10}},
			rounds: 2,
			want:   map[string]string{"n2": reasonReference},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, voters(tt.nodes...)...)
			for id, failed := range tt.ref {
				r.refFailed[id] = failed
			}
			for id, age := range tt.refAge {
				r.refAge[id] = age
			}
			r.dev = tt.dev
			r.tick() // the first tick only starts the leadership
			for range tt.rounds {
				r.step()
			}
			got := r.impairedProposals()
			if len(got) != len(tt.want) {
				t.Fatalf("impaired = %v, want %v", got, tt.want)
			}
			for id, reason := range tt.want {
				if got[id] != reason {
					t.Errorf("node %s: reason %q, want %q (all: %v)", id, got[id], reason, got)
				}
				if !r.a.Exclude(id) {
					t.Errorf("Exclude(%s) = false, want true", id)
				}
			}
		})
	}
}

// TestRecoveryAfterTwoMinutesWithoutSignal checks the hysteresis on the way
// back: a node stays impaired until two minutes after its last signal.
func TestRecoveryAfterTwoMinutesWithoutSignal(t *testing.T) {
	r := newRig(t, voters("n1", "n2", "n3")...)
	r.refFailed["n1"] = true
	r.refFailed["n2"] = false
	r.refFailed["n3"] = false
	r.tick()
	r.step()
	r.step() // impaired; the last signal is at this assessment
	if !r.a.Exclude("n1") {
		t.Fatal("n1 not impaired after two rounds with a signal")
	}
	lastSignal := r.now

	r.refFailed["n1"] = false
	for range 9 { // 15 s to 135 s after the last signal: still impaired
		r.step()
		since := r.now.Sub(lastSignal)
		if since < 2*time.Minute && !r.a.Exclude("n1") {
			t.Fatalf("%v after the last signal: n1 connected, want impaired", since)
		}
		if since >= 2*time.Minute {
			break
		}
	}
	if r.a.Exclude("n1") {
		t.Fatalf("n1 still impaired %v after its last signal", r.now.Sub(lastSignal))
	}
	last := r.app.cmds[len(r.app.cmds)-1].GetSetNodeConnectivity()
	if last.GetNodeId() != "n1" || last.GetImpaired() || last.GetReason() != "" {
		t.Errorf("last proposal = %v, want n1 connected", last)
	}
}

// TestSingleRoundDoesNotHoldImpaired checks that a signal of one round does
// not make a node impaired and that it stays connected afterwards.
func TestSingleRoundDoesNotHoldImpaired(t *testing.T) {
	r := newRig(t, voters("n1", "n2", "n3")...)
	r.refFailed["n1"] = true
	r.refFailed["n2"] = false
	r.refFailed["n3"] = false
	r.tick()
	r.step()
	r.refFailed["n1"] = false
	r.step()
	r.step()
	if r.a.Exclude("n1") || len(r.app.cmds) != 0 {
		t.Errorf("single round made n1 impaired: proposals %d", len(r.app.cmds))
	}
}

// TestNotEnabledKeepsDecisionInMemory checks that a command level below 3
// keeps the impaired decision in memory and proposes it again until it is accepted.
func TestNotEnabledKeepsDecisionInMemory(t *testing.T) {
	r := newRig(t, voters("n1", "n2", "n3")...)
	r.refFailed["n1"] = true
	r.refFailed["n2"] = false
	r.refFailed["n3"] = false
	r.app.err = &cluster.CommandError{Code: cluster.CodeNotEnabled, Message: "level 2"}
	r.tick()
	r.step()
	r.step()
	if !r.a.Exclude("n1") {
		t.Fatal("decision not kept in memory after not_enabled")
	}
	r.step()
	if len(r.app.cmds) != 2 || r.app.cmds[0] != r.app.cmds[1] {
		t.Fatalf("proposals = %d, want the same command retried", len(r.app.cmds))
	}

	r.app.err = nil
	r.step()
	n := len(r.app.cmds)
	r.step()
	if len(r.app.cmds) != n {
		t.Errorf("accepted decision proposed again: %d -> %d", n, len(r.app.cmds))
	}
	if !r.a.Exclude("n1") {
		t.Error("decision lost after it was accepted")
	}
}

// TestFollowerDoesNothing checks that a voter that does not lead neither
// decides nor proposes, and that a former leader drops its decisions.
func TestFollowerDoesNothing(t *testing.T) {
	r := newRig(t, voters("n1", "n2", "n3")...)
	r.refFailed["n1"] = true
	r.refFailed["n2"] = false
	r.refFailed["n3"] = false
	r.leader = false
	r.tick()
	r.step()
	r.step()
	r.step()
	if len(r.app.cmds) != 0 || r.a.Exclude("n1") {
		t.Fatalf("follower proposed %d commands", len(r.app.cmds))
	}

	r.leader = true
	r.tick()
	r.step()
	r.step()
	if !r.a.Exclude("n1") {
		t.Fatal("leader did not exclude n1")
	}
	r.leader = false
	r.step()
	if r.a.Exclude("n1") {
		t.Error("former leader kept its in-memory decision")
	}
}

// TestRegistryStateCountsAfterLeaderChange checks that a new leader starts
// from the registry state and recovers an impaired node only after two
// minutes without a signal, counted from its first assessment.
func TestRegistryStateCountsAfterLeaderChange(t *testing.T) {
	nodes := voters("n1", "n2", "n3")
	nodes[1].ConnectivityImpaired = true
	nodes[1].ConnectivityReason = reasonDeviation
	r := newRig(t, nodes...)
	r.refFailed["n2"] = false
	r.refFailed["n3"] = false
	// A voter reads the registry while it is a follower, so it is current when
	// it becomes leader.
	r.leader = false
	r.tick()
	if !r.a.Exclude("n2") {
		t.Fatal("registry state not used as a follower")
	}
	r.leader = true
	r.tick()
	// The first assessment is at 15 s; two minutes later the node is connected.
	for range 10 {
		r.step()
		if r.now.Sub(base) < 15*time.Second+2*time.Minute && !r.a.Exclude("n2") {
			t.Fatalf("n2 connected at %v, want impaired", r.now.Sub(base))
		}
	}
	if r.a.Exclude("n2") {
		t.Fatal("n2 still impaired long after the grace of two minutes")
	}
	got := r.app.cmds[len(r.app.cmds)-1].GetSetNodeConnectivity()
	if got.GetNodeId() != "n2" || got.GetImpaired() {
		t.Errorf("last proposal = %v, want n2 connected", got)
	}
}

// TestDecisionsWaitForFirstRound checks that the first assessment as leader
// decides nothing, even with a signal present.
func TestDecisionsWaitForFirstRound(t *testing.T) {
	r := newRig(t, voters("n1", "n2", "n3")...)
	r.refFailed["n1"] = true
	r.refFailed["n2"] = false
	r.refFailed["n3"] = false
	r.tick()
	if len(r.app.cmds) != 0 {
		t.Fatalf("decision in the first tick: %d proposals", len(r.app.cmds))
	}
}
