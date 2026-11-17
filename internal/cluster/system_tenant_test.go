// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"testing"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// TestSystemTenantReserved: only the system actor creates the system tenant,
// and it never holds monitors (ADR 0025).
func TestSystemTenantReserved(t *testing.T) {
	h := newAlertFSM(t, t.TempDir())

	byUser, err := NewPutTenant("sys-by-user", store.SystemTenantID, TenantDocument{Name: "System", Plan: "system"})
	if err != nil {
		t.Fatal(err)
	}
	byUser.Actor = &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u1"}
	h.do(byUser, 1000, ErrInvalidCommand)

	bySystem, err := NewPutTenant("sys-by-system", store.SystemTenantID, TenantDocument{Name: "System", Plan: "system"})
	if err != nil {
		t.Fatal(err)
	}
	bySystem.Actor = &pb.Actor{Kind: pb.Actor_KIND_SYSTEM}
	h.do(bySystem, 1000, nil)

	monitor, err := NewPutMonitor("", store.SystemTenantID, "m1",
		MonitorDocument{Name: "m1", Type: "http", IntervalSeconds: 60, TimeoutSeconds: 5, Assignment: store.Assignment{Mode: "all"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	h.do(monitor, 2000, ErrInvalidCommand)
}
