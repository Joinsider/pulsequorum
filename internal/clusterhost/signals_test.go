// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package clusterhost

import (
	"context"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/sysalert"
)

// ADR 0025, section 2: every voter reports its incarnation, disk, WAL sizes and
// certificate expiry in GetStatus, and the leader records them per peer.
func TestPeerAndSelfSignals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	n1, err := startNodeWithClock(ctx, t, nil, "", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n1 leader", 10*time.Second, n1.host.Node.IsLeader)
	token := func() string {
		tok, _, err := n1.host.Authority.CreateJoinToken(ctx, pki.RoleVoter, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	n2, err := startNodeWithClock(ctx, t, []string{n1.addr}, token(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n2 voter", 30*time.Second, func() bool { return isVoter(n1, n2.id) })

	eventually(t, "signals of n2 on n1", 20*time.Second, func() bool {
		s, ok := n1.host.PeerSignals()[n2.id]
		return ok && s.HasSystemFields() && !s.CertNotAfter.IsZero()
	})
	s := n1.host.PeerSignals()[n2.id]
	if s.Incarnation != n2.metrics.Incarnation() {
		t.Errorf("peer incarnation = %q, want %q", s.Incarnation, n2.metrics.Incarnation())
	}
	// The test nodes configure neither the disk nor the WAL functions: the disk
	// is unknown (negative), the WAL sizes read zero.
	if s.DiskFreeRatio >= 0 {
		t.Errorf("peer disk free ratio = %v, want unknown for a node without the function", s.DiskFreeRatio)
	}
	if s.StateWALBytes != 0 || s.MetricsWALBytes != 0 {
		t.Errorf("peer WAL sizes = %d / %d, want 0 without the function", s.StateWALBytes, s.MetricsWALBytes)
	}
	if s.ReplicationLagMs < 0 {
		t.Errorf("peer replication lag = %d, want a value of a caught up voter", s.ReplicationLagMs)
	}

	self := n1.host.SelfSignals()
	if self.NodeID != n1.id || self.Incarnation != n1.metrics.Incarnation() {
		t.Errorf("self signals = %+v", self)
	}
	if self.DiskFreeRatio >= 0 {
		t.Errorf("self disk free ratio = %v, want unknown for a node without the function", self.DiskFreeRatio)
	}
	if self.CertNotAfter.IsZero() {
		t.Errorf("self certificate expiry missing")
	}
	if self.RaftState != "Leader" {
		t.Errorf("self raft state = %q, want Leader", self.RaftState)
	}
}

// The signal tracker drops answers older than clockMaxAge.
func TestSignalTrackerExpires(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	var tr signalTracker
	tr.record("fresh", sysalert.Signals{NodeID: "fresh", AnsweredAt: now})
	tr.record("stale", sysalert.Signals{NodeID: "stale", AnsweredAt: now.Add(-clockMaxAge - time.Second)})
	got := tr.snapshot(now)
	if _, ok := got["fresh"]; !ok || len(got) != 1 {
		t.Errorf("snapshot = %v, want only the fresh peer", got)
	}
}
