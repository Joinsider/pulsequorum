// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package scheduler

import (
	"log/slog"

	"github.com/joinsider/pulsequorum/internal/store"
)

// TenantGate hands Target only the monitors of tenants that may run: a
// suspended tenant rests its checks and alerts, and the monitors of a tenant
// that no longer exists are gone (ADR 0029). The system tenant is never
// suspended. Tenants returns the tenant list; if that fails, Target keeps the
// previous monitors.
type TenantGate struct {
	Target  Syncer
	Tenants func() ([]store.Tenant, error)
	Log     *slog.Logger
}

// Sync implements Syncer.
func (g TenantGate) Sync(monitors []store.Monitor) {
	tenants, err := g.Tenants()
	if err != nil {
		g.Log.Error("reading the tenants failed, keeping the current monitors", "err", err)
		return
	}
	g.Target.Sync(ActiveMonitors(monitors, tenants))
}

// ActiveMonitors returns the monitors whose tenant is listed in tenants and
// not suspended, in their original order. The system tenant always counts as
// active.
func ActiveMonitors(monitors []store.Monitor, tenants []store.Tenant) []store.Monitor {
	active := ActiveTenantIDs(tenants)
	out := make([]store.Monitor, 0, len(monitors))
	for _, m := range monitors {
		if active[m.TenantID] {
			out = append(out, m)
		}
	}
	return out
}

// ActiveTenantIDs returns the set of tenants that are listed and not suspended.
func ActiveTenantIDs(tenants []store.Tenant) map[string]bool {
	active := make(map[string]bool, len(tenants))
	for _, t := range tenants {
		if !t.Suspended || t.ID == store.SystemTenantID {
			active[t.ID] = true
		}
	}
	return active
}
