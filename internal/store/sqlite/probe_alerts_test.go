// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sqlite

import (
	"context"
	"testing"

	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/store"
)

func TestProbeAlertsAndCounts(t *testing.T) {
	ctx := context.Background()
	s, err := OpenState(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	// PutProbeAlert validation and CRUD + CountTenantPrivateProbes + CountOpenTenantJoinTokens.
	err = s.Update(ctx, func(tx store.StateTx) error {
		if err := tx.PutProbeAlert(ctx, store.ProbeAlert{}); err == nil {
			t.Fatal("expected error for empty probe alert tenant/probe ID")
		}
		if err := tx.PutTenant(ctx, store.Tenant{ID: "t1", Name: "Tenant 1", Plan: "starter", CreatedMs: 100, UpdatedMs: 100}); err != nil {
			return err
		}
		if err := tx.PutNode(ctx, store.Node{
			ID:            "probe-1",
			Name:          "Probe 1",
			Role:          store.NodeRoleProbe,
			OwnerTenantID: "t1",
			Healthy:       true,
			UpdatedMs:     100,
		}); err != nil {
			return err
		}
		if err := tx.InsertJoinToken(ctx, store.JoinToken{
			ID:         "tok-1",
			SecretHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			Role:       string(pki.RoleProbe),
			TenantID:   "t1",
			ExpiresMs:  2000,
			CreatedMs:  100,
		}); err != nil {
			return err
		}
		if err := tx.PutProbeAlert(ctx, store.ProbeAlert{
			TenantID:  "t1",
			ProbeID:   "probe-1",
			Active:    true,
			SinceMs:   150,
			UpdatedMs: 150,
		}); err != nil {
			return err
		}
		if err := tx.PutProbeAlert(ctx, store.ProbeAlert{
			TenantID:  "t1",
			ProbeID:   "probe-2",
			Active:    false,
			SinceMs:   120,
			UpdatedMs: 160,
		}); err != nil {
			return err
		}

		got, err := tx.GetProbeAlert(ctx, "t1", "probe-1")
		if err != nil || !got.Active || got.SinceMs != 150 {
			t.Fatalf("GetProbeAlert = (%+v, %v)", got, err)
		}
		nProbes, err := tx.CountTenantPrivateProbes(ctx, "t1")
		if err != nil || nProbes != 1 {
			t.Fatalf("tx.CountTenantPrivateProbes = (%d, %v), want 1", nProbes, err)
		}
		nTokens, err := tx.CountOpenTenantJoinTokens(ctx, "t1", 1000)
		if err != nil || nTokens != 1 {
			t.Fatalf("tx.CountOpenTenantJoinTokens = (%d, %v), want 1", nTokens, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	allTenant, err := s.ListProbeAlerts(ctx, "t1", false)
	if err != nil || len(allTenant) != 2 {
		t.Fatalf("ListProbeAlerts(activeOnly=false) = (%+v, %v)", allTenant, err)
	}
	activeTenant, err := s.ListProbeAlerts(ctx, "t1", true)
	if err != nil || len(activeTenant) != 1 || activeTenant[0].ProbeID != "probe-1" {
		t.Fatalf("ListProbeAlerts(activeOnly=true) = (%+v, %v)", activeTenant, err)
	}

	allAlerts, err := s.ListAllProbeAlerts(ctx, false)
	if err != nil || len(allAlerts) != 2 {
		t.Fatalf("ListAllProbeAlerts(activeOnly=false) = (%+v, %v)", allAlerts, err)
	}
	activeAlerts, err := s.ListAllProbeAlerts(ctx, true)
	if err != nil || len(activeAlerts) != 1 {
		t.Fatalf("ListAllProbeAlerts(activeOnly=true) = (%+v, %v)", activeAlerts, err)
	}

	nProbes, err := s.CountTenantPrivateProbes(ctx, "t1")
	if err != nil || nProbes != 1 {
		t.Fatalf("s.CountTenantPrivateProbes = (%d, %v), want 1", nProbes, err)
	}
	nTokens, err := s.CountOpenTenantJoinTokens(ctx, "t1", 1000)
	if err != nil || nTokens != 1 {
		t.Fatalf("s.CountOpenTenantJoinTokens = (%d, %v), want 1", nTokens, err)
	}

	if err := s.Update(ctx, func(tx store.StateTx) error {
		return tx.DeleteProbeAlertsByProbe(ctx, "probe-1")
	}); err != nil {
		t.Fatal(err)
	}
	afterDel, err := s.ListAllProbeAlerts(ctx, true)
	if err != nil || len(afterDel) != 0 {
		t.Fatalf("after DeleteProbeAlertsByProbe = (%+v, %v)", afterDel, err)
	}
}

func TestMetricsStreamDeletesAndPurge(t *testing.T) {
	ctx := context.Background()
	m, err := OpenMetrics(ctx, t.TempDir(), MetricsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	if _, err := m.PurgeTenant(ctx, "", 10); err == nil {
		t.Fatal("expected error for empty tenant ID in PurgeTenant")
	}
	if _, err := m.PurgeTenant(ctx, "t1", 0); err == nil {
		t.Fatal("expected error for non-positive batchSize in PurgeTenant")
	}
	if err := m.MarkTenantPurged(ctx, "", 1000); err == nil {
		t.Fatal("expected error for empty tenant ID in MarkTenantPurged")
	}
	if err := m.MarkTenantPurged(ctx, "t1", 1000); err != nil {
		t.Fatalf("MarkTenantPurged: %v", err)
	}
	purged, err := m.PurgedTenants(ctx)
	if err != nil || len(purged) != 1 || purged[0] != "t1" {
		t.Fatalf("PurgedTenants = (%v, %v), want [t1]", purged, err)
	}

	inc := m.Incarnation()
	hbs := []store.Heartbeat{
		{NodeID: "p1", Incarnation: inc, Seq: 1, TenantID: "t1", MonitorID: "m1", TsMs: 1000, Status: store.CheckUp, LatencyMs: 10},
		{NodeID: "p1", Incarnation: inc, Seq: 2, TenantID: "t1", MonitorID: "m1", TsMs: 2000, Status: store.CheckUp, LatencyMs: 12},
		{NodeID: "p1", Incarnation: inc, Seq: 3, TenantID: "t1", MonitorID: "m1", TsMs: 3000, Status: store.CheckDown, LatencyMs: 50},
		{NodeID: "p1", Incarnation: inc, Seq: 4, TenantID: "t1", MonitorID: "m1", TsMs: 4000, Status: store.CheckUp, LatencyMs: 11},
	}
	if _, err := m.InsertHeartbeats(ctx, hbs); err != nil {
		t.Fatalf("InsertHeartbeats: %v", err)
	}

	minSeq, err := m.MinSeq(ctx, "p1", inc)
	if err != nil || minSeq != 1 {
		t.Fatalf("MinSeq = (%d, %v), want 1", minSeq, err)
	}

	count, oldest, err := m.UnackedStats(ctx, "p1", inc, 1)
	if err != nil || count != 3 || oldest != 2000 {
		t.Fatalf("UnackedStats = (%d, %d, %v), want (3, 2000, nil)", count, oldest, err)
	}

	if _, err := m.DeleteStreamHeartbeatsUpTo(ctx, "p1", inc, 1, 0); err == nil {
		t.Fatal("expected error for limit <= 0 in DeleteStreamHeartbeatsUpTo")
	}
	nDel, err := m.DeleteStreamHeartbeatsUpTo(ctx, "p1", inc, 1, 10)
	if err != nil || nDel != 1 {
		t.Fatalf("DeleteStreamHeartbeatsUpTo = (%d, %v), want 1", nDel, err)
	}

	if _, _, err := m.DeleteStreamHeartbeatsOlderThan(ctx, "p1", inc, 1, 2500, 0); err == nil {
		t.Fatal("expected error for limit <= 0 in DeleteStreamHeartbeatsOlderThan")
	}
	nOlder, maxOlderSeq, err := m.DeleteStreamHeartbeatsOlderThan(ctx, "p1", inc, 1, 2500, 10)
	if err != nil || nOlder != 1 || maxOlderSeq != 2 {
		t.Fatalf("DeleteStreamHeartbeatsOlderThan = (%d, %d, %v), want (1, 2, nil)", nOlder, maxOlderSeq, err)
	}
	// Calling again when none match returns (0, 0, nil).
	nOlder, maxOlderSeq, err = m.DeleteStreamHeartbeatsOlderThan(ctx, "p1", inc, 1, 2500, 10)
	if err != nil || nOlder != 0 || maxOlderSeq != 0 {
		t.Fatalf("DeleteStreamHeartbeatsOlderThan empty = (%d, %d, %v), want (0, 0, nil)", nOlder, maxOlderSeq, err)
	}

	if _, _, _, err := m.DeleteOldestStreamHeartbeats(ctx, "p1", inc, 2, 0); err == nil {
		t.Fatal("expected error for limit <= 0 in DeleteOldestStreamHeartbeats")
	}
	del, unackedDel, maxUnacked, err := m.DeleteOldestStreamHeartbeats(ctx, "p1", inc, 2, 1)
	if err != nil || del != 1 || unackedDel != 1 || maxUnacked != 3 {
		t.Fatalf("DeleteOldestStreamHeartbeats = (%d, %d, %d, %v), want (1, 1, 3, nil)", del, unackedDel, maxUnacked, err)
	}

	size, err := m.DBSizeBytes(ctx)
	if err != nil || size <= 0 {
		t.Fatalf("DBSizeBytes = (%d, %v)", size, err)
	}
	if err := m.IncrementalVacuum(ctx, 1); err != nil {
		t.Fatalf("IncrementalVacuum: %v", err)
	}
}
