// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sqlite

import (
	"context"
	"fmt"
	"testing"

	"github.com/joinsider/pulsequorum/internal/store"
)

// TestFingerprintLevelGates covers ADR 0029, ADR 0031 and ADR 0018: the plans,
// the tombstones and the column tenants.source enter the fingerprint from
// command level 8 on, the API tokens and the audit log from level 9 on, so
// that a rolling update does not look like divergence.
// Rows are changed directly in SQL, bypassing the command level on purpose.
func TestFingerprintLevelGates(t *testing.T) {
	changes := []struct {
		name string
		sql  string
		// from is the lowest command level at which the change enters the fingerprint.
		from uint32
	}{
		{"plan limits", `UPDATE plans SET limits_json = '{"maxMonitors":5}' WHERE id = 'default'`, 8},
		{"plan name", `UPDATE plans SET name = 'Renamed' WHERE id = 'default'`, 8},
		{"tenant source", `UPDATE tenants SET source = 'auto' WHERE id = 't1'`, 8},
		{"tombstone", `INSERT INTO deleted_tenants (id, deleted_ms) VALUES ('gone', 5)`, 8},
		{"api token", `INSERT INTO api_tokens (id, tenant_id, scope, name, role, secret_hash, created_ms, expires_ms,
			created_by_kind, created_by_id) VALUES ('tok_1', 't1', 'tenant', 'ci', 'tenant-editor', 'hash', 1, 9000000000000,
			'user', 'usr-1')`, 9},
		{"audit entry", `INSERT INTO audit_log (raft_index, sub, at_ms, tenant_id, actor_kind, action)
			VALUES (7, 0, 1, 't1', 'user', 'monitor.create')`, 9},
		{"node owner_tenant_id", `UPDATE nodes SET owner_tenant_id = 't1' WHERE id = 'p1'`, 10},
		{"node incarnation", `UPDATE nodes SET incarnation = 'inc-1' WHERE id = 'p1'`, 10},
		{"tenant name (control, always hashed)", `UPDATE tenants SET name = 'Other' WHERE id = 't1'`, 0},
	}
	for _, level := range []uint32{7, 8, 9, 10} {
		for _, c := range changes {
			t.Run(fmt.Sprintf("level %d: %s", level, c.name), func(t *testing.T) {
				ctx := context.Background()
				s := openState(t, t.TempDir())
				err := s.Update(ctx, func(tx store.StateTx) error {
					if err := tx.SetCommandLevel(ctx, level); err != nil {
						return err
					}
					if err := tx.PutTenant(ctx, store.Tenant{ID: "t1", Name: "T1", Plan: "default", Source: "admin", CreatedMs: 1, UpdatedMs: 1}); err != nil {
						return err
					}
					return tx.PutNode(ctx, store.Node{ID: "p1", Name: "probe1", Role: store.NodeRoleProbe, Healthy: true, HealthChangedMs: 1, RegisteredMs: 1, UpdatedMs: 1})
				})
				if err != nil {
					t.Fatal(err)
				}
				before, err := s.Fingerprint(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.db.writer.ExecContext(ctx, c.sql); err != nil {
					t.Fatalf("%s: %v", c.sql, err)
				}
				after, err := s.Fingerprint(ctx)
				if err != nil {
					t.Fatal(err)
				}
				changed := before != after
				want := level >= c.from
				if changed != want {
					t.Errorf("fingerprint changed = %v at level %d, want %v", changed, level, want)
				}
			})
		}
	}
}
