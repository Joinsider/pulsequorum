// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package store

import (
	"context"
	"fmt"
)

// AllMonitors returns the monitors of all tenants.
func AllMonitors(ctx context.Context, state State) ([]Monitor, error) {
	tenants, err := state.ListTenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	var out []Monitor
	for _, t := range tenants {
		q := MonitorQuery{Limit: 500}
		for {
			page, err := state.ListMonitors(ctx, t.ID, q)
			if err != nil {
				return nil, fmt.Errorf("list monitors of %s: %w", t.ID, err)
			}
			out = append(out, page.Items...)
			if page.NextAfterID == "" {
				break
			}
			q.AfterID = page.NextAfterID
		}
	}
	return out, nil
}
