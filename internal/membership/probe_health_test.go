// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package membership

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/cluster"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

type probeUpdateEvent struct {
	at     time.Duration
	active map[string]bool
}

func TestProbeHealthTracker(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	tests := []struct {
		name     string
		leader   bool
		updates  []probeUpdateEvent
		now      time.Duration
		registry []store.Node
		want     map[string]bool
	}{
		{
			name:   "without leadership nothing is decided",
			leader: false,
			updates: []probeUpdateEvent{
				{at: 0, active: map[string]bool{"p1": true}},
			},
			now: 90 * time.Second,
			registry: []store.Node{
				{ID: "p1", Role: store.NodeRoleProbe, Healthy: false},
			},
		},
		{
			name:   "grace period on new leader suppresses healthy transition",
			leader: true,
			updates: []probeUpdateEvent{
				{at: 0, active: map[string]bool{"p1": true}},
			},
			now: 29 * time.Second,
			registry: []store.Node{
				{ID: "p1", Role: store.NodeRoleProbe, Healthy: false},
			},
		},
		{
			name:   "30s continuous session after grace marks probe healthy",
			leader: true,
			updates: []probeUpdateEvent{
				{at: 0, active: map[string]bool{"p1": true}},
			},
			now: 30 * time.Second,
			registry: []store.Node{
				{ID: "p1", Role: store.NodeRoleProbe, Healthy: false},
			},
			want: map[string]bool{"p1": true},
		},
		{
			name:   "brief connect less than 30s does not flip healthy",
			leader: true,
			updates: []probeUpdateEvent{
				{at: 10 * time.Second, active: map[string]bool{"p1": true}},
			},
			now: 39 * time.Second,
			registry: []store.Node{
				{ID: "p1", Role: store.NodeRoleProbe, Healthy: false},
			},
		},
		{
			name:   "brief connect interrupted by disconnect resets healthy timer",
			leader: true,
			updates: []probeUpdateEvent{
				{at: 0, active: map[string]bool{"p1": true}},
				{at: 20 * time.Second, active: map[string]bool{}},
				{at: 25 * time.Second, active: map[string]bool{"p1": true}},
			},
			now: 50 * time.Second,
			registry: []store.Node{
				{ID: "p1", Role: store.NodeRoleProbe, Healthy: false},
			},
		},
		{
			name:   "never seen active since leaderSince becomes unhealthy after 60s",
			leader: true,
			now:    60 * time.Second,
			registry: []store.Node{
				{ID: "p1", Role: store.NodeRoleProbe, Healthy: true},
			},
			want: map[string]bool{"p1": false},
		},
		{
			name:   "never seen active since leaderSince stays healthy at 59s",
			leader: true,
			now:    59 * time.Second,
			registry: []store.Node{
				{ID: "p1", Role: store.NodeRoleProbe, Healthy: true},
			},
		},
		{
			name:   "60s offline after active session marks probe unhealthy",
			leader: true,
			updates: []probeUpdateEvent{
				{at: 0, active: map[string]bool{"p1": true}},
				{at: 10 * time.Second, active: map[string]bool{}},
			},
			now: 70 * time.Second,
			registry: []store.Node{
				{ID: "p1", Role: store.NodeRoleProbe, Healthy: true},
			},
			want: map[string]bool{"p1": false},
		},
		{
			name:   "brief disconnect less than 60s does not flip unhealthy",
			leader: true,
			updates: []probeUpdateEvent{
				{at: 0, active: map[string]bool{"p1": true}},
				{at: 10 * time.Second, active: map[string]bool{}},
				{at: 69 * time.Second, active: map[string]bool{"p1": true}},
			},
			now: 70 * time.Second,
			registry: []store.Node{
				{ID: "p1", Role: store.NodeRoleProbe, Healthy: true},
			},
		},
		{
			name:   "already matching health produces no change",
			leader: true,
			updates: []probeUpdateEvent{
				{at: 0, active: map[string]bool{"p1": true}},
			},
			now: 90 * time.Second,
			registry: []store.Node{
				{ID: "p1", Role: store.NodeRoleProbe, Healthy: true},
				{ID: "p2", Role: store.NodeRoleProbe, Healthy: false},
			},
		},
		{
			name:   "voters are ignored",
			leader: true,
			now:    90 * time.Second,
			registry: []store.Node{
				{ID: "v1", Role: store.NodeRoleVoter, Healthy: true},
				{ID: "v2", Role: store.NodeRoleVoter, Healthy: false},
				{ID: "p1", Role: store.NodeRoleProbe, Healthy: true},
			},
			want: map[string]bool{"p1": false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tr ProbeHealthTracker
			if tt.leader {
				tr.BecameLeader(base)
			}
			for _, u := range tt.updates {
				tr.Update(base.Add(u.at), u.active)
			}
			got := tr.Decide(base.Add(tt.now), tt.registry)
			if !maps.Equal(got, tt.want) {
				t.Errorf("Decide() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProbeHealthTrackerReset(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	var tr ProbeHealthTracker
	tr.BecameLeader(base)
	tr.Update(base, map[string]bool{"p1": true})
	tr.Reset()

	registry := []store.Node{{ID: "p1", Role: store.NodeRoleProbe, Healthy: false}}
	if got := tr.Decide(base.Add(90*time.Second), registry); len(got) != 0 {
		t.Errorf("after Reset without leadership: Decide() = %v, want none", got)
	}

	tr.BecameLeader(base.Add(100 * time.Second))
	tr.Update(base.Add(100*time.Second), map[string]bool{"p1": true})
	if got := tr.Decide(base.Add(129*time.Second), registry); len(got) != 0 {
		t.Errorf("during new grace: Decide() = %v, want none", got)
	}
	if got := tr.Decide(base.Add(130*time.Second), registry); !got["p1"] {
		t.Errorf("after new grace: Decide() = %v, want p1 healthy", got)
	}
}

type fakeProbeHealthNode struct {
	mu      sync.Mutex
	leader  bool
	applied []*pb.SetNodeHealth
}

func (n *fakeProbeHealthNode) IsLeader() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.leader
}

func (n *fakeProbeHealthNode) setLeader(v bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.leader = v
}

func (n *fakeProbeHealthNode) Apply(_ context.Context, cmd *pb.Command) (cluster.ApplyResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if s := cmd.GetSetNodeHealth(); s != nil {
		n.applied = append(n.applied, s)
	}
	return cluster.ApplyResult{}, nil
}

func (n *fakeProbeHealthNode) takeApplied() []*pb.SetNodeHealth {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := n.applied
	n.applied = nil
	return out
}

type fakeProbeHealthState struct {
	store.State
	mu    sync.Mutex
	nodes []store.Node
}

func (s *fakeProbeHealthState) ListNodes(context.Context) ([]store.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.Node, len(s.nodes))
	copy(out, s.nodes)
	return out, nil
}

func (s *fakeProbeHealthState) setNodeHealthy(id string, healthy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.nodes {
		if s.nodes[i].ID == id {
			s.nodes[i].Healthy = healthy
		}
	}
}

func TestProbeHealthWatcher(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	now := base
	node := &fakeProbeHealthNode{leader: false}
	state := &fakeProbeHealthState{
		nodes: []store.Node{
			{ID: "v1", Role: store.NodeRoleVoter, Healthy: true},
			{ID: "p2", Role: store.NodeRoleProbe, Healthy: true},
			{ID: "p1", Role: store.NodeRoleProbe, Healthy: false},
		},
	}
	sessions := map[string]ProbeSessionInfo{
		"p1": {NodeID: "p1", ConnectedVoterID: "v1"},
	}

	w := &ProbeHealthWatcher{
		Node:     node,
		State:    state,
		Sessions: func() map[string]ProbeSessionInfo { return sessions },
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      func() time.Time { return now },
		Interval: 10 * time.Millisecond,
	}

	ctx := t.Context()

	// Not leader: no proposals.
	w.tick(ctx, now)
	if got := node.takeApplied(); len(got) != 0 {
		t.Fatalf("not leader: got %d proposals, want 0", len(got))
	}

	// Become leader at t=0: grace period suppresses changes until t=30s.
	node.setLeader(true)
	w.tick(ctx, now)
	if got := node.takeApplied(); len(got) != 0 {
		t.Fatalf("at leader start: got %d proposals, want 0", len(got))
	}

	// At t=30s: p1 has been continuously connected for 30s -> healthy.
	now = base.Add(30 * time.Second)
	w.tick(ctx, now)
	got := node.takeApplied()
	if len(got) != 1 || got[0].GetNodeId() != "p1" || !got[0].GetHealthy() {
		t.Fatalf("at 30s: got %v, want p1->true", got)
	}
	state.setNodeHealthy("p1", true)

	// At t=60s: p2 was never seen active since t=0 -> unhealthy.
	now = base.Add(60 * time.Second)
	w.tick(ctx, now)
	got = node.takeApplied()
	if len(got) != 1 || got[0].GetNodeId() != "p2" || got[0].GetHealthy() {
		t.Fatalf("at 60s: got %v, want p2->false", got)
	}
	state.setNodeHealthy("p2", false)

	// Step down and back up: state resets and new grace applies.
	node.setLeader(false)
	now = base.Add(65 * time.Second)
	w.tick(ctx, now)
	node.setLeader(true)
	sessions = map[string]ProbeSessionInfo{}
	now = base.Add(70 * time.Second)
	w.tick(ctx, now)
	if got := node.takeApplied(); len(got) != 0 {
		t.Fatalf("after re-election in grace: got %d proposals, want 0", len(got))
	}

	// Verify Run exits cleanly on context cancellation.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		w.Run(runCtx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on context cancellation")
	}
}
