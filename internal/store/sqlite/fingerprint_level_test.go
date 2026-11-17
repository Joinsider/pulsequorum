// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sqlite

import (
	"context"
	"fmt"
	"testing"

	"github.com/joinsider/uptimecluster/internal/store"
)

// TestFingerprintTenantAdminLevelGate covers ADR 0029 and ADR 0018: the plans,
// the tombstones and the column tenants.source enter the fingerprint only from
// command level 8 on, so that a rolling update does not look like divergence.
// Rows are changed directly in SQL, bypassing the command level on purpose.
func TestFingerprintTenantAdminLevelGate(t *testing.T) {
	changes := []struct {
		name string
		sql  string
		// always is true for a change that the fingerprint covers at every level.
		always bool
	}{
		{"plan limits", `UPDATE plans SET limits_json = '{"maxMonitors":5}' WHERE id = 'default'`, false},
		{"plan name", `UPDATE plans SET name = 'Renamed' WHERE id = 'default'`, false},
		{"tenant source", `UPDATE tenants SET source = 'auto' WHERE id = 't1'`, false},
		{"tombstone", `INSERT INTO deleted_tenants (id, deleted_ms) VALUES ('gone', 5)`, false},
		{"tenant name (control, always hashed)", `UPDATE tenants SET name = 'Other' WHERE id = 't1'`, true},
	}
	for _, level := range []uint32{7, 8} {
		for _, c := range changes {
			t.Run(fmt.Sprintf("level %d: %s", level, c.name), func(t *testing.T) {
				ctx := context.Background()
				s := openState(t, t.TempDir())
				err := s.Update(ctx, func(tx store.StateTx) error {
					if err := tx.SetCommandLevel(ctx, level); err != nil {
						return err
					}
					return tx.PutTenant(ctx, store.Tenant{ID: "t1", Name: "T1", Plan: "default", Source: "admin", CreatedMs: 1, UpdatedMs: 1})
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
				want := c.always || level >= 8
				if changed != want {
					t.Errorf("fingerprint changed = %v at level %d, want %v", changed, level, want)
				}
			})
		}
	}
}
