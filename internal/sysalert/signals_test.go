// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sysalert

import (
	"testing"
	"time"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
)

func TestFromStatus(t *testing.T) {
	answered := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	st := &pb.GetStatusResponse{
		RaftState: "Follower", LeaderId: "n1", AppliedIndex: 7, CommitIndex: 9, LastContactMs: 40,
		Incarnation: "inc", DiskFreeRatio: 0.3, StateWalBytes: 1, MetricsWalBytes: 2,
		CertNotAfterMs: expiry.UnixMilli(), ReplicationLagMs: -1, FingerprintIndex: 5, Fingerprint: "fp",
	}
	got := FromStatus("n2", st, answered)
	want := Signals{
		NodeID: "n2", AnsweredAt: answered, RaftState: "Follower", LeaderID: "n1",
		AppliedIndex: 7, CommitIndex: 9, LastContactMs: 40, Incarnation: "inc", DiskFreeRatio: 0.3,
		StateWALBytes: 1, MetricsWALBytes: 2, CertNotAfter: expiry, ReplicationLagMs: -1,
		FingerprintIndex: 5, Fingerprint: "fp",
	}
	if got.NodeID != want.NodeID || !got.AnsweredAt.Equal(want.AnsweredAt) || got.LeaderID != want.LeaderID ||
		got.AppliedIndex != want.AppliedIndex || got.CommitIndex != want.CommitIndex || got.LastContactMs != want.LastContactMs ||
		got.Incarnation != want.Incarnation || got.DiskFreeRatio != want.DiskFreeRatio ||
		got.StateWALBytes != want.StateWALBytes || got.MetricsWALBytes != want.MetricsWALBytes ||
		!got.CertNotAfter.Equal(want.CertNotAfter) || got.ReplicationLagMs != want.ReplicationLagMs ||
		got.FingerprintIndex != want.FingerprintIndex || got.Fingerprint != want.Fingerprint {
		t.Errorf("FromStatus = %+v, want %+v", got, want)
	}
	if !got.HasSystemFields() {
		t.Errorf("answer with incarnation has no system fields")
	}

	old := FromStatus("n3", &pb.GetStatusResponse{AppliedIndex: 3}, answered)
	if old.HasSystemFields() || !old.CertNotAfter.IsZero() {
		t.Errorf("older node counts as reporting ADR 0025 fields: %+v", old)
	}
}

func TestRaftStale(t *testing.T) {
	tests := []struct {
		name   string
		s      Signals
		commit uint64
		want   bool
	}{
		{name: "in step", s: Signals{AppliedIndex: 100, LastContactMs: 50}, commit: 100, want: false},
		{name: "gap at the limit", s: Signals{AppliedIndex: 0}, commit: StaleApplyGap, want: false},
		{name: "gap above the limit", s: Signals{AppliedIndex: 0}, commit: StaleApplyGap + 1, want: true},
		{name: "applied ahead of the commit seen", s: Signals{AppliedIndex: 500}, commit: 100, want: false},
		{name: "contact at the limit", s: Signals{LastContactMs: StaleLastContact.Milliseconds()}, want: false},
		{name: "contact above the limit", s: Signals{LastContactMs: StaleLastContact.Milliseconds() + 1}, want: true},
	}
	for _, tt := range tests {
		if got := RaftStale(tt.s, tt.commit); got != tt.want {
			t.Errorf("%s: RaftStale = %v, want %v", tt.name, got, tt.want)
		}
	}
}
