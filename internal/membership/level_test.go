// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package membership

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// TestTargetLevel covers ADR 0018 (FA-CLU-12): the level is the minimum over
// all Raft members, non-voters included, and unknown or zero versions count as
// the base level.
func TestTargetLevel(t *testing.T) {
	voter := func(id string) cluster.Member { return cluster.Member{ID: id, Voter: true} }
	nonvoter := func(id string) cluster.Member { return cluster.Member{ID: id} }
	node := func(id string, v uint32) store.Node { return store.Node{ID: id, MaxCommandVersion: v} }

	tests := []struct {
		name     string
		members  []cluster.Member
		registry []store.Node
		want     uint32
	}{
		{
			name:     "all equal",
			members:  []cluster.Member{voter("a"), voter("b")},
			registry: []store.Node{node("a", 3), node("b", 3)},
			want:     3,
		},
		{
			name:     "one voter behind",
			members:  []cluster.Member{voter("a"), voter("b"), voter("c")},
			registry: []store.Node{node("a", 3), node("b", 3), node("c", 2)},
			want:     2,
		},
		{
			name:     "unregistered voter counts as base level",
			members:  []cluster.Member{voter("a"), voter("b")},
			registry: []store.Node{node("a", 3)},
			want:     cluster.BaseCommandLevel,
		},
		{
			name:     "non-voters catching up count",
			members:  []cluster.Member{voter("a"), nonvoter("p")},
			registry: []store.Node{node("a", 3), node("p", 2)},
			want:     2,
		},
		{
			name:     "zero version counts as base level",
			members:  []cluster.Member{voter("a"), voter("b")},
			registry: []store.Node{node("a", 3), node("b", 0)},
			want:     cluster.BaseCommandLevel,
		},
		{
			name:     "only a non-voter",
			members:  []cluster.Member{nonvoter("p")},
			registry: []store.Node{node("p", 3)},
			want:     3,
		},
		{
			name:     "empty configuration",
			registry: []store.Node{node("a", 3)},
			want:     cluster.BaseCommandLevel,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := targetLevel(tc.members, tc.registry); got != tc.want {
				t.Errorf("targetLevel = %d, want %d", got, tc.want)
			}
		})
	}
}

// fakeLevelNode is a leader or follower that records the proposed commands.
// Apply stores the proposed level as the level of the node.
type fakeLevelNode struct {
	leader  bool
	members []cluster.Member
	level   uint32
	applied []*pb.Command
	err     error
}

func (f *fakeLevelNode) IsLeader() bool { return f.leader }
func (f *fakeLevelNode) Members() ([]cluster.Member, uint64, error) {
	return f.members, 1, nil
}
func (f *fakeLevelNode) CommandLevel(context.Context) (uint32, error) { return f.level, nil }
func (f *fakeLevelNode) Apply(_ context.Context, cmd *pb.Command) (cluster.ApplyResult, error) {
	f.applied = append(f.applied, cmd)
	if f.err != nil {
		return cluster.ApplyResult{}, f.err
	}
	f.level = cmd.GetSetFeatureLevel().GetLevel()
	return cluster.ApplyResult{}, nil
}

// fakeLevelState serves a fixed node registry; other store calls are not used.
type fakeLevelState struct {
	store.State
	nodes []store.Node
}

func (f fakeLevelState) ListNodes(context.Context) ([]store.Node, error) { return f.nodes, nil }

// TestLevelWatcherTick covers ADR 0018: only the leader proposes, and only a
// level above the current one.
func TestLevelWatcherTick(t *testing.T) {
	voters := []cluster.Member{{ID: "a", Voter: true}, {ID: "b", Voter: true}}
	registry := []store.Node{{ID: "a", MaxCommandVersion: 3}, {ID: "b", MaxCommandVersion: 3}}
	lagging := []store.Node{{ID: "a", MaxCommandVersion: 3}, {ID: "b", MaxCommandVersion: 2}}

	tests := []struct {
		name   string
		leader bool
		level  uint32
		nodes  []store.Node
		// want is the level proposed, 0 for no proposal.
		want uint32
	}{
		{name: "leader proposes a higher level", leader: true, level: 2, nodes: registry, want: 3},
		{name: "follower never proposes", leader: false, level: 2, nodes: registry},
		{name: "target equal to current: no proposal", leader: true, level: 3, nodes: registry},
		{name: "a lagging voter keeps the level", leader: true, level: 2, nodes: lagging},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := &fakeLevelNode{leader: tc.leader, members: voters, level: tc.level}
			w := &LevelWatcher{Node: node, State: fakeLevelState{nodes: tc.nodes}, Log: slog.New(slog.DiscardHandler)}
			w.tick(context.Background())
			if tc.want == 0 {
				if len(node.applied) != 0 {
					t.Fatalf("proposed %d commands, want none", len(node.applied))
				}
				return
			}
			if len(node.applied) != 1 {
				t.Fatalf("proposed %d commands, want 1", len(node.applied))
			}
			if got := node.applied[0].GetSetFeatureLevel().GetLevel(); got != tc.want {
				t.Errorf("proposed level %d, want %d", got, tc.want)
			}
		})
	}
}

// TestLevelWatcherRetries: a failed proposal is repeated on the next tick.
func TestLevelWatcherRetries(t *testing.T) {
	node := &fakeLevelNode{leader: true, members: []cluster.Member{{ID: "a", Voter: true}}, level: 2, err: errors.New("boom")}
	w := &LevelWatcher{
		Node:  node,
		State: fakeLevelState{nodes: []store.Node{{ID: "a", MaxCommandVersion: 3}}},
		Log:   slog.New(slog.DiscardHandler),
	}
	w.tick(context.Background())
	node.err = nil
	w.tick(context.Background())
	if len(node.applied) != 2 {
		t.Fatalf("proposed %d commands, want 2", len(node.applied))
	}
	if node.level != 3 {
		t.Errorf("level = %d, want 3 after the retry", node.level)
	}
}
