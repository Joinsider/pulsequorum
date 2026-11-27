// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import "testing"

func TestIsStale(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		st   Status
		want bool
	}{
		{name: "leader is never stale", st: Status{RaftState: "Leader", LeaderID: "n1", LastContactMs: 99_999, CommitIndex: 5000}, want: false},
		{name: "leader without contact timestamp", st: Status{RaftState: "Leader", LeaderID: "n1"}, want: false},
		{name: "follower up to date", st: Status{RaftState: "Follower", LeaderID: "n1", LastContactMs: 120, AppliedIndex: 10, CommitIndex: 10}, want: false},
		{name: "follower without leader", st: Status{RaftState: "Follower", AppliedIndex: 10, CommitIndex: 10}, want: true},
		{name: "candidate without leader", st: Status{RaftState: "Candidate"}, want: true},
		{name: "contact exactly at threshold", st: Status{RaftState: "Follower", LeaderID: "n1", LastContactMs: staleContactMs}, want: false},
		{name: "contact above threshold", st: Status{RaftState: "Follower", LeaderID: "n1", LastContactMs: staleContactMs + 1}, want: true},
		{name: "apply lag exactly at threshold", st: Status{RaftState: "Follower", LeaderID: "n1", AppliedIndex: 100, CommitIndex: 100 + staleApplyLag}, want: false},
		{name: "apply lag above threshold", st: Status{RaftState: "Follower", LeaderID: "n1", AppliedIndex: 100, CommitIndex: 100 + staleApplyLag + 1}, want: true},
		{name: "applied ahead of commit", st: Status{RaftState: "Follower", LeaderID: "n1", AppliedIndex: 200, CommitIndex: 100}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsStale(tc.st); got != tc.want {
				t.Errorf("IsStale(%+v) = %v, want %v", tc.st, got, tc.want)
			}
		})
	}
}
