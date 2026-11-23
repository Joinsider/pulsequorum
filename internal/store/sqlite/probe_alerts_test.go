// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/joinsider/pulsequorum/internal/store"
)

func TestProbeAlertsAndTenantCounts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openState(t, t.TempDir())

	err := s.Update(ctx, func(tx store.StateTx) error {
		if err := tx.PutProbeAlert(ctx, store.ProbeAlert{}); err == nil {
			t.Fatal("PutProbeAlert with empty tenant/probe ID: want error")
		}
		if err := tx.PutTenant(ctx, store.Tenant{ID: "t1", Name: "T1", Plan: "default", CreatedMs: 1, UpdatedMs: 1}); err != nil {
			return err
		}
		if err := tx.PutTenant(ctx, store.Tenant{ID: "t2", Name: "T2", Plan: "default", CreatedMs: 1, UpdatedMs: 1}); err != nil {
			return err
		}
		if err := tx.PutProbeAlert(ctx, store.ProbeAlert{TenantID: "t1", ProbeID: "p1", Active: true, SinceMs: 100, UpdatedMs: 100}); err != nil {
			return err
		}
		if err := tx.PutProbeAlert(ctx, store.ProbeAlert{TenantID: "t1", ProbeID: "p2", Active: false, SinceMs: 90, UpdatedMs: 110}); err != nil {
			return err
		}
		if err := tx.PutProbeAlert(ctx, store.ProbeAlert{TenantID: "t2", ProbeID: "p3", Active: true, SinceMs: 200, UpdatedMs: 200}); err != nil {
			return err
		}
		if err := tx.PutNode(ctx, store.Node{ID: "p1", Role: store.NodeRoleProbe, OwnerTenantID: "t1", RegisteredMs: 1, UpdatedMs: 1}); err != nil {
			return err
		}
		if err := tx.PutNode(ctx, store.Node{ID: "p-pub", Role: store.NodeRoleProbe, OwnerTenantID: "", RegisteredMs: 1, UpdatedMs: 1}); err != nil {
			return err
		}
		if err := tx.PutNode(ctx, store.Node{ID: "p3", Role: store.NodeRoleProbe, OwnerTenantID: "t2", RegisteredMs: 1, UpdatedMs: 1}); err != nil {
			return err
		}
		// Open token (unexpired, unused)
		if err := tx.InsertJoinToken(ctx, store.JoinToken{ID: "jt-open", SecretHash: "h1", Role: "probe", TenantID: "t1", ExpiresMs: 1000, CreatedMs: 1}); err != nil {
			return err
		}
		// Expired token
		if err := tx.InsertJoinToken(ctx, store.JoinToken{ID: "jt-exp", SecretHash: "h2", Role: "probe", TenantID: "t1", ExpiresMs: 500, CreatedMs: 1}); err != nil {
			return err
		}
		// Used token
		if err := tx.InsertJoinToken(ctx, store.JoinToken{ID: "jt-used", SecretHash: "h3", Role: "probe", TenantID: "t1", ExpiresMs: 1000, CreatedMs: 1}); err != nil {
			return err
		}
		if err := tx.MarkJoinTokenUsed(ctx, "jt-used", 10, "p1"); err != nil {
			return err
		}
		// Other tenant's open token
		return tx.InsertJoinToken(ctx, store.JoinToken{ID: "jt-t2", SecretHash: "h4", Role: "probe", TenantID: "t2", ExpiresMs: 1000, CreatedMs: 1})
	})
	if err != nil {
		t.Fatal(err)
	}

	t1All, err := s.ListProbeAlerts(ctx, "t1", false)
	if err != nil || len(t1All) != 2 {
		t.Fatalf("ListProbeAlerts(t1, false) = %+v, %v", t1All, err)
	}
	t1Active, err := s.ListProbeAlerts(ctx, "t1", true)
	if err != nil || len(t1Active) != 1 || t1Active[0].ProbeID != "p1" {
		t.Fatalf("ListProbeAlerts(t1, true) = %+v, %v", t1Active, err)
	}
	allActive, err := s.ListAllProbeAlerts(ctx, true)
	if err != nil || len(allActive) != 2 {
		t.Fatalf("ListAllProbeAlerts(true) = %+v, %v", allActive, err)
	}

	if n, err := s.CountTenantPrivateProbes(ctx, "t1"); err != nil || n != 1 {
		t.Fatalf("CountTenantPrivateProbes(t1) = %d, %v, want 1", n, err)
	}
	if n, err := s.CountOpenTenantJoinTokens(ctx, "t1", 500); err != nil || n != 1 {
		t.Fatalf("CountOpenTenantJoinTokens(t1, 500) = %d, %v, want 1", n, err)
	}

	if err := s.Update(ctx, func(tx store.StateTx) error {
		if err := tx.DeleteProbeAlertsByProbe(ctx, "p1"); err != nil {
			return err
		}
		_, err := tx.GetProbeAlert(ctx, "t1", "p1")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetProbeAlert after delete = %v, want ErrNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMetricsStreamDeletesAndPurge(t *testing.T) {
	t.Parallel()
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
