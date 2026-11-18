// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"testing"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
)

// TestAPITokenEntityGuard covers ADR 0031, section 1 at the FSM: an API token
// may create, change and delete only the tenant's monitors, channels and
// maintenance windows. Users, tenants, join tokens and the cluster objects are
// rejected for a token actor, whatever the handler checked.
func TestAPITokenEntityGuard(t *testing.T) {
	tokenActor := &pb.Actor{Kind: pb.Actor_KIND_API_TOKEN, Id: tokID(1), Name: "ci"}
	rawPut := func(kind pb.EntityKind, id string) *pb.Command {
		return &pb.Command{
			RequestId: "raw-put-" + kind.String(),
			TenantId:  "t1",
			Actor:     tokenActor,
			Payload: &pb.Command_PutEntity{PutEntity: &pb.PutEntity{
				Kind: kind, Id: id, DocumentJson: []byte("{}"),
			}},
		}
	}
	rawDelete := func(kind pb.EntityKind, id string) *pb.Command {
		return &pb.Command{
			RequestId: "raw-delete-" + kind.String(),
			TenantId:  "t1",
			Actor:     tokenActor,
			Payload: &pb.Command_DeleteEntity{DeleteEntity: &pb.DeleteEntity{
				Kind: kind, Id: id,
			}},
		}
	}
	tests := []struct {
		name string
		cmd  *pb.Command
		code string
	}{
		{"put user", rawPut(pb.EntityKind_ENTITY_KIND_USER, "u1"), CodeInvalid},
		{"put tenant", rawPut(pb.EntityKind_ENTITY_KIND_TENANT, "t1"), CodeInvalid},
		{"put join token", rawPut(pb.EntityKind_ENTITY_KIND_JOIN_TOKEN, "j1"), CodeInvalid},
		{"put secret key", rawPut(pb.EntityKind_ENTITY_KIND_SECRET_KEY, "k1"), CodeInvalid},
		{"put signing key", rawPut(pb.EntityKind_ENTITY_KIND_SIGNING_KEY, "s1"), CodeInvalid},
		{"put cluster CA", rawPut(pb.EntityKind_ENTITY_KIND_CLUSTER_CA, "ca1"), CodeInvalid},
		{"delete user", rawDelete(pb.EntityKind_ENTITY_KIND_USER, "u1"), CodeInvalid},
		{"delete join token", rawDelete(pb.EntityKind_ENTITY_KIND_JOIN_TOKEN, "j1"), CodeInvalid},
		{"delete tenant", rawDelete(pb.EntityKind_ENTITY_KIND_TENANT, "t1"), CodeInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tokenFSM(t)
			checkCode(t, h.apply(tt.cmd, 2000).Err, tt.code)
		})
	}

	t.Run("put monitor is allowed", func(t *testing.T) {
		h := tokenFSM(t)
		cmd, err := NewPutMonitor("token-monitor", "t1", "m1", monitorDoc("Web m1"), 0)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Actor = tokenActor
		checkCode(t, h.apply(cmd, 2000).Err, "")
	})

	t.Run("put channel is allowed", func(t *testing.T) {
		h := tokenFSM(t)
		cmd, err := NewPutChannel("token-channel", "t1", "ch1", channel("down"), 0)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Actor = tokenActor
		checkCode(t, h.apply(cmd, 2000).Err, "")
	})

	t.Run("delete monitor is allowed", func(t *testing.T) {
		h := tokenFSM(t)
		h.putMonitorAt(1100, "create-m1", "t1", "m1", 60, 0, "")
		cmd := NewDeleteMonitor("token-delete", "t1", "m1")
		cmd.Actor = tokenActor
		checkCode(t, h.apply(cmd, 2000).Err, "")
	})
}
