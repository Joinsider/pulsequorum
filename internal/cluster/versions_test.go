// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"testing"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// ADR 0018: every payload of the oneof has a minimum version, so a new payload
// cannot silently get the version of the binary that adds it.
func TestMinVersionCoversEveryPayload(t *testing.T) {
	oneof := (&pb.Command{}).ProtoReflect().Descriptor().Oneofs().ByName("payload")
	if oneof == nil {
		t.Fatal("Command has no payload oneof")
	}
	fields := oneof.Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		t.Run(string(fd.Name()), func(t *testing.T) {
			m := (&pb.Command{}).ProtoReflect()
			m.Set(fd, m.NewField(fd))
			cmd := m.Interface().(*pb.Command)
			if cmd.GetPayload() == nil {
				t.Fatalf("payload %s not set", fd.Name())
			}
			if v := minVersion(cmd); v == 0 || v > CommandVersion {
				t.Errorf("minVersion(%s) = %d, want 1..%d", fd.Name(), v, CommandVersion)
			}
		})
	}
}

func TestMinVersion(t *testing.T) {
	tests := []struct {
		name string
		cmd  *pb.Command
		want uint32
	}{
		{"nil payload", &pb.Command{}, 0},
		{"delete entity", &pb.Command{Payload: &pb.Command_DeleteEntity{DeleteEntity: &pb.DeleteEntity{}}}, 1},
		{"revoke node", &pb.Command{Payload: &pb.Command_RevokeNode{RevokeNode: &pb.RevokeNode{}}}, 1},
		{"register node", &pb.Command{Payload: &pb.Command_RegisterNode{RegisterNode: &pb.RegisterNode{}}}, 2},
		{"set node health", &pb.Command{Payload: &pb.Command_SetNodeHealth{SetNodeHealth: &pb.SetNodeHealth{}}}, 2},
		{"set feature level", &pb.Command{Payload: &pb.Command_SetFeatureLevel{SetFeatureLevel: &pb.SetFeatureLevel{Level: 3}}}, 2},
		{"set node connectivity", &pb.Command{Payload: &pb.Command_SetNodeConnectivity{SetNodeConnectivity: &pb.SetNodeConnectivity{}}}, 3},
		{"constructor set node connectivity", NewSetNodeConnectivity("node-1", true, "reference"), 3},
		{"constructor register node", NewRegisterNode("r", store.Node{ID: "node-1", Role: store.NodeRoleVoter}), 2},
		{"constructor delete monitor", NewDeleteMonitor("r", "default", "m1"), 1},
		{"constructor set feature level", NewSetFeatureLevel("r", 3), 2},
		{"clear monitor flapping", &pb.Command{Payload: &pb.Command_ClearMonitorFlapping{ClearMonitorFlapping: &pb.ClearMonitorFlapping{}}}, 4},
		{"constructor clear monitor flapping", NewClearMonitorFlapping("t1", "m1", 1000), 4},
		{"report degraded alerts", &pb.Command{Payload: &pb.Command_ReportDegradedAlerts{ReportDegradedAlerts: &pb.ReportDegradedAlerts{}}}, 5},
		{"resolve unconfirmed alerts", &pb.Command{Payload: &pb.Command_ResolveUnconfirmedAlerts{ResolveUnconfirmedAlerts: &pb.ResolveUnconfirmedAlerts{}}}, 5},
		{"record monitor history", &pb.Command{Payload: &pb.Command_RecordMonitorHistory{RecordMonitorHistory: &pb.RecordMonitorHistory{}}}, 5},
		{"constructor report degraded alerts", NewReportDegradedAlerts("node-1", nil), 5},
		{"constructor resolve unconfirmed alerts", NewResolveUnconfirmedAlerts(nil), 5},
		{"constructor record monitor history", NewRecordMonitorHistory("t1", "m1", store.StatusUp, 1, 2, nil), 5},
		{"constructor set monitor state stays at 1", NewSetMonitorState(StateChange{TenantID: "t1", MonitorID: "m1", From: store.StatusUp, To: store.StatusDown, SinceMs: 1, Incident: &store.Incident{ID: "i"}, Reporters: []string{"n1"}}), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := minVersion(tt.cmd); got != tt.want {
				t.Errorf("minVersion() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestStampSetsVersionFromPayload(t *testing.T) {
	cmd := stamp(&pb.Command{Version: 9, Payload: &pb.Command_SetFeatureLevel{SetFeatureLevel: &pb.SetFeatureLevel{Level: 2}}})
	if cmd.GetVersion() != 2 {
		t.Errorf("stamped version = %d, want 2", cmd.GetVersion())
	}
}
