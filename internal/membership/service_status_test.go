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
