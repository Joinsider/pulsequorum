// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package auth

import (
	"context"
	"testing"

	"github.com/joinsider/pulsequorum/internal/store"
)

// TestSystemTenantPrincipalRejected: a principal of the reserved system tenant
// is never accepted for tenant requests (ADR 0025).
func TestSystemTenantPrincipalRejected(t *testing.T) {
	for _, tc := range []struct {
		tenant string
		want   bool
	}{{"default", true}, {"", false}, {store.SystemTenantID, false}} {
		ctx := WithPrincipal(context.Background(), Principal{TenantID: tc.tenant, Roles: []Role{RolePlatformAdmin}})
		if _, ok := FromContext(ctx); ok != tc.want {
			t.Errorf("tenant %q: ok = %v, want %v", tc.tenant, ok, tc.want)
		}
	}
}
