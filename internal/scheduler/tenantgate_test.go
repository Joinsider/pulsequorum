// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package scheduler

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/joinsider/pulsequorum/internal/store"
)

type recordingSyncer struct {
	calls [][]store.Monitor
}

func (r *recordingSyncer) Sync(m []store.Monitor) { r.calls = append(r.calls, m) }

func TestTenantGateDropsSuspendedAndDeletedTenants(t *testing.T) {
	monitors := []store.Monitor{
		{TenantID: "a", ID: "m1"},
		{TenantID: "b", ID: "m2"},
		{TenantID: "gone", ID: "m3"},
		{TenantID: store.SystemTenantID, ID: "m4"},
	}
	tests := []struct {
		name    string
		tenants []store.Tenant
		want    []string
	}{
		{
			name:    "all active",
			tenants: []store.Tenant{{ID: "a"}, {ID: "b"}, {ID: store.SystemTenantID}},
			want:    []string{"m1", "m2", "m4"},
		},
		{
			name:    "suspended tenant rests",
			tenants: []store.Tenant{{ID: "a", Suspended: true}, {ID: "b"}, {ID: store.SystemTenantID}},
			want:    []string{"m2", "m4"},
		},
		{
			name:    "system tenant is never suspended",
			tenants: []store.Tenant{{ID: "a"}, {ID: "b"}, {ID: store.SystemTenantID, Suspended: true}},
			want:    []string{"m1", "m2", "m4"},
		},
		{
			name:    "unknown tenant is gone",
			tenants: []store.Tenant{{ID: "a"}},
			want:    []string{"m1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := &recordingSyncer{}
			gate := TenantGate{Target: target, Tenants: func() ([]store.Tenant, error) { return tt.tenants, nil },
				Log: slog.New(slog.DiscardHandler)}
			gate.Sync(monitors)
			if len(target.calls) != 1 {
				t.Fatalf("target synced %d times, want 1", len(target.calls))
			}
			var got []string
			for _, m := range target.calls[0] {
				got = append(got, m.ID)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("monitors = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("monitors = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestTenantGateKeepsPreviousMonitorsOnError(t *testing.T) {
	target := &recordingSyncer{}
	gate := TenantGate{Target: target, Tenants: func() ([]store.Tenant, error) { return nil, errors.New("db down") },
		Log: slog.New(slog.DiscardHandler)}
	gate.Sync([]store.Monitor{{TenantID: "a", ID: "m1"}})
	if len(target.calls) != 0 {
		t.Fatalf("target was synced despite the error: %v", target.calls)
	}
}
