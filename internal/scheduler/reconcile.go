// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/joinsider/uptimecluster/internal/store"
)

// Reconcile keeps the scheduler in line with state.db: it loads all monitors
// when changes signals and at least every interval, so a configuration change
// is active within seconds (FA-SCH-06). In phase 1 a node runs every monitor;
// assignment to nodes follows in phase 2.
func Reconcile(ctx context.Context, state store.State, changes <-chan struct{}, s *Scheduler, interval time.Duration, log *slog.Logger) {
	load := func() {
		monitors, err := AllMonitors(ctx, state)
		if err != nil {
			if ctx.Err() == nil {
				log.Error("loading monitors for the scheduler failed", "err", err)
			}
			return
		}
		s.Sync(monitors)
	}
	load()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-changes:
		case <-ticker.C:
		}
		load()
	}
}

// AllMonitors returns the monitors of all tenants.
func AllMonitors(ctx context.Context, state store.State) ([]store.Monitor, error) {
	tenants, err := state.ListTenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	var out []store.Monitor
	for _, t := range tenants {
		q := store.MonitorQuery{Limit: 500}
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
