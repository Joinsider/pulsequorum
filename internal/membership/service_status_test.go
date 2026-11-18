// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package membership

import (
	"context"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/selfcheck"
)

// statusNode answers Status only; any other Node method panics through the nil embedding.
type statusNode struct {
	Node
	st cluster.Status
}

func (n statusNode) Status() cluster.Status { return n.st }

// ADR 0022: GetStatus reports the self-check of the node; without targets the
// fields stay zero, which older nodes also send.
func TestGetStatusReportsSelfCheck(t *testing.T) {
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		selfCheck func() selfcheck.Status
		want      *pb.GetStatusResponse
	}{
		{
			name: "no targets configured",
			want: &pb.GetStatusResponse{},
		},
		{
			name:      "targets failed in the latest round",
			selfCheck: func() selfcheck.Status { return selfcheck.Status{Configured: true, Failed: true, AtMs: 42} },
			want:      &pb.GetStatusResponse{SelfCheckConfigured: true, SelfCheckFailed: true, SelfCheckAtMs: 42},
		},
		{
			name:      "targets reachable",
			selfCheck: func() selfcheck.Status { return selfcheck.Status{Configured: true, AtMs: 7} },
			want:      &pb.GetStatusResponse{SelfCheckConfigured: true, SelfCheckAtMs: 7},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Service{
				Node:      statusNode{st: cluster.Status{NodeID: "n1", RaftState: "Leader"}},
				SelfCheck: tt.selfCheck,
				Clock:     func() time.Time { return now },
			}
			got, err := s.GetStatus(context.Background(), &pb.GetStatusRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if got.GetSelfCheckConfigured() != tt.want.GetSelfCheckConfigured() ||
				got.GetSelfCheckFailed() != tt.want.GetSelfCheckFailed() ||
				got.GetSelfCheckAtMs() != tt.want.GetSelfCheckAtMs() {
				t.Errorf("self check = %v/%v/%d, want %v/%v/%d", got.GetSelfCheckConfigured(), got.GetSelfCheckFailed(), got.GetSelfCheckAtMs(),
					tt.want.GetSelfCheckConfigured(), tt.want.GetSelfCheckFailed(), tt.want.GetSelfCheckAtMs())
			}
			if got.GetNodeId() != "n1" || got.GetRaftState() != "Leader" || got.GetWallClockUnixMs() != now.UnixMilli() {
				t.Errorf("raft fields lost: %v", got)
			}
		})
	}
}

// ADR 0025: GetStatus reports the signals of the node; a nil function or a zero
// certificate expiry leaves the field unset, which the leader reads as no signal.
func TestGetStatusReportsOwnSignals(t *testing.T) {
	expiry := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	full := &Service{
		Node: statusNode{st: cluster.Status{NodeID: "n1", RaftState: "Follower"}},
		Own: OwnSignals{
			Incarnation:      func() string { return "inc-1" },
			DiskFreeRatio:    func() float64 { return 0.4 },
			WALBytes:         func() (int64, int64) { return 100, 200 },
			CertNotAfter:     func() time.Time { return expiry },
			ReplicationLagMs: func() int64 { return -1 },
			Checkpoint:       func() (uint64, string) { return 77, "fp" },
		},
	}
	got, err := full.GetStatus(context.Background(), &pb.GetStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := &pb.GetStatusResponse{
		Incarnation: "inc-1", DiskFreeRatio: 0.4, StateWalBytes: 100, MetricsWalBytes: 200,
		CertNotAfterMs: expiry.UnixMilli(), ReplicationLagMs: -1, FingerprintIndex: 77, Fingerprint: "fp",
	}
	if got.GetIncarnation() != want.GetIncarnation() || got.GetDiskFreeRatio() != want.GetDiskFreeRatio() ||
		got.GetStateWalBytes() != want.GetStateWalBytes() || got.GetMetricsWalBytes() != want.GetMetricsWalBytes() ||
		got.GetCertNotAfterMs() != want.GetCertNotAfterMs() || got.GetReplicationLagMs() != want.GetReplicationLagMs() ||
		got.GetFingerprintIndex() != want.GetFingerprintIndex() || got.GetFingerprint() != want.GetFingerprint() {
		t.Errorf("GetStatus = %v, want the signals %v", got, want)
	}

	// An older binary, or a node without the functions, reports the zero values.
	bare := &Service{Node: statusNode{st: cluster.Status{NodeID: "n2"}}, Own: OwnSignals{
		CertNotAfter: func() time.Time { return time.Time{} },
	}}
	got, err = bare.GetStatus(context.Background(), &pb.GetStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetIncarnation() != "" || got.GetDiskFreeRatio() != 0 || got.GetCertNotAfterMs() != 0 || got.GetFingerprintIndex() != 0 {
		t.Errorf("bare GetStatus = %v, want no ADR 0025 signals", got)
	}
}

// ADR 0030: GetStatus reports whether SSRF protection is on for the node, in
// both states.
func TestGetStatusReportsSSRFProtection(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		s := &Service{
			Node:           statusNode{st: cluster.Status{NodeID: "n1", RaftState: "Follower"}},
			SSRFProtection: enabled,
		}
		got, err := s.GetStatus(context.Background(), &pb.GetStatusRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if got.SsrfProtection == nil {
			t.Fatal("ssrf_protection not sent; peers would read it as unknown")
		}
		if got.GetSsrfProtection() != enabled {
			t.Errorf("ssrf_protection = %v, want %v", got.GetSsrfProtection(), enabled)
		}
	}
}
