// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/store"
)

// FA-CLU-05: a follower forwards only while its leader was heard recently.
func TestLeaderContactStale(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	const heartbeat = time.Second

	tests := []struct {
		name string
		last time.Time
		want bool
	}{
		{"never contacted", time.Time{}, true},
		{"just now", now, false},
		{"within twice the heartbeat", now.Add(-2*heartbeat + time.Millisecond), false},
		{"exactly twice the heartbeat", now.Add(-2 * heartbeat), false},
		{"older than twice the heartbeat", now.Add(-2*heartbeat - time.Millisecond), true},
		{"long ago", now.Add(-time.Hour), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := leaderContactStale(tc.last, now, heartbeat); got != tc.want {
				t.Errorf("leaderContactStale() = %v, want %v", got, tc.want)
			}
		})
	}
}

// ADR 0020: the leader stamps commands with the clock of its node, so a node
// whose clock is skewed records its own time in the replicated state.
func TestNodeStampsCommandsWithItsClock(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	st := openStore(t, dir)
	skewed := time.Date(2031, 5, 6, 7, 8, 9, 0, time.UTC)
	n, err := Start(ctx, Config{NodeID: "33333333-3333-3333-3333-333333333333", Advertise: "node1:9443", DataDir: dir,
		Timeout: 200 * time.Millisecond, Logger: slog.New(slog.DiscardHandler), RaftLogLevel: "error",
		Clock: func() time.Time { return skewed }}, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Shutdown() }()
	if err := n.WaitLeader(ctx); err != nil {
		t.Fatal(err)
	}

	const id = "44444444-4444-4444-4444-444444444444"
	if _, err := n.Apply(ctx, NewRegisterNode("register:test", store.Node{ID: id, Name: "skewed", Role: store.NodeRoleVoter, Version: "test"})); err != nil {
		t.Fatal(err)
	}
	nodes, err := st.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got store.Node
	for _, r := range nodes {
		if r.ID == id {
			got = r
		}
	}
	if want := skewed.UnixMilli(); got.RegisteredMs != want {
		t.Errorf("RegisteredMs = %d, want the node clock %d", got.RegisteredMs, want)
	}
}
