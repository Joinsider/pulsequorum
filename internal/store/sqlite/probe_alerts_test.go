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
	ctx := context.Background()
	s := openState(t, t.TempDir())

	err := s.Update(ctx, func(tx store.StateTx) error {
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
