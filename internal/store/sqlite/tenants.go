// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite/statedb"
)

// Tenant sources (ADR 0029).
const (
	tenantSourceBootstrap = "bootstrap"
	tenantSourceAdmin     = "admin"
	tenantSourceAuto      = "auto"
)

// normalizeSource maps an empty source to bootstrap and rejects unknown values.
func normalizeSource(source string) (string, error) {
	switch source {
	case "":
		return tenantSourceBootstrap, nil
	case tenantSourceBootstrap, tenantSourceAdmin, tenantSourceAuto:
		return source, nil
	}
	return "", fmt.Errorf("tenant source %q is not admin, auto or bootstrap", source)
}

// TenantUsage counts what a tenant uses of its limits (ADR 0029).
func (s *StateStore) TenantUsage(ctx context.Context, tenantID string) (store.TenantUsage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return tenantUsage(ctx, s.q, tenantID)
}

// GetPlan returns a plan or ErrNotFound.
func (s *StateStore) GetPlan(ctx context.Context, id string) (store.Plan, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, err := s.q.GetPlan(ctx, id)
	if err != nil {
		return store.Plan{}, mapErr(err)
	}
	return planFromRow(r), nil
}

// ListPlans returns all plans ordered by ID.
func (s *StateStore) ListPlans(ctx context.Context) ([]store.Plan, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.q.ListPlans(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.Plan, 0, len(rows))
	for _, r := range rows {
		out = append(out, planFromRow(r))
	}
	return out, nil
}

// ListDeletedTenants returns the tombstones ordered by ID.
func (s *StateStore) ListDeletedTenants(ctx context.Context) ([]store.DeletedTenant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.q.ListDeletedTenants(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.DeletedTenant, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.DeletedTenant{ID: r.ID, DeletedMs: r.DeletedMs})
	}
	return out, nil
}

// CountTenants returns the number of tenants created with the source.
func (t *stateTx) CountTenants(ctx context.Context, source string) (int, error) {
	n, err := t.q.CountTenantsBySource(ctx, source)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(n), nil
}

// CountTenantsWithPlan returns the number of tenants on the plan.
func (t *stateTx) CountTenantsWithPlan(ctx context.Context, planID string) (int, error) {
	n, err := t.q.CountTenantsWithPlan(ctx, planID)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(n), nil
}

// TenantUsage counts the tenant's monitors and channels (ADR 0029).
func (t *stateTx) TenantUsage(ctx context.Context, tenantID string) (store.TenantUsage, error) {
	return tenantUsage(ctx, t.q, tenantID)
}

// IsTenantDeleted reports whether a tombstone exists for the ID.
func (t *stateTx) IsTenantDeleted(ctx context.Context, id string) (bool, error) {
	n, err := t.q.IsTenantDeleted(ctx, id)
	if err != nil {
		return false, mapErr(err)
	}
	return n > 0, nil
}

// tenantDeletes are the per-table deletes of DeleteTenant, in the order they
// run: children before parents, and the tenants row last (see DeleteTenant).
// Every state table with a tenant_id column is listed; the sqlite tests check
// the list against sqlite_master.
var tenantDeletes = []func(q *statedb.Queries, ctx context.Context, tenantID string) error{
	(*statedb.Queries).DeleteTenantAlerts,
	(*statedb.Queries).DeleteTenantMonitorChannels,
	(*statedb.Queries).DeleteTenantSessions,
	(*statedb.Queries).DeleteTenantUsers,
	(*statedb.Queries).DeleteTenantMaintenanceWindows,
	(*statedb.Queries).DeleteTenantChannels,
	(*statedb.Queries).DeleteTenantIncidents,
	(*statedb.Queries).DeleteTenantStatusHistory,
	(*statedb.Queries).DeleteTenantMonitorStates,
	(*statedb.Queries).DeleteTenantMonitorTags,
	(*statedb.Queries).DeleteTenantMonitors,
	(*statedb.Queries).DeleteTenantUnconfirmedAlerts,
	(*statedb.Queries).DeleteTenantJoinTokens,
	(*statedb.Queries).DeleteTenantAPITokens,
	(*statedb.Queries).DeleteTenantAudit,
}

// DeleteTenant removes the tenant and every row of it in state.db and records
// a tombstone, inside the caller's transaction (ADR 0029). An unknown tenant
// is ErrNotFound and nothing is changed.
func (t *stateTx) DeleteTenant(ctx context.Context, id string, deletedMs int64) error {
	// Defense in depth: the FSM already refuses these (ADR 0025, 0029).
	if id == store.SystemTenantID || id == "default" {
		return fmt.Errorf("delete tenant %s: reserved tenant", id)
	}
	if _, err := t.q.GetTenant(ctx, id); err != nil {
		return mapErr(err)
	}
	for _, del := range tenantDeletes {
		if err := del(t.q, ctx, id); err != nil {
			return fmt.Errorf("delete tenant %s rows: %w", id, mapErr(err))
		}
	}
	n, err := t.q.DeleteTenantRow(ctx, id)
	if err != nil {
		return fmt.Errorf("delete tenant %s: %w", id, mapErr(err))
	}
	if n == 0 {
		return store.ErrNotFound
	}
	if err := t.q.InsertDeletedTenant(ctx, statedb.InsertDeletedTenantParams{ID: id, DeletedMs: deletedMs}); err != nil {
		return fmt.Errorf("tombstone tenant %s: %w", id, mapErr(err))
	}
	return nil
}

// GetPlan returns a plan or ErrNotFound.
func (t *stateTx) GetPlan(ctx context.Context, id string) (store.Plan, error) {
	r, err := t.q.GetPlan(ctx, id)
	if err != nil {
		return store.Plan{}, mapErr(err)
	}
	return planFromRow(r), nil
}

// PutPlan creates or replaces a plan; created_ms of an existing plan is kept.
func (t *stateTx) PutPlan(ctx context.Context, p store.Plan) error {
	if p.ID == "" {
		return errors.New("put plan: empty ID")
	}
	limits := string(p.Limits)
	if limits == "" {
		limits = "{}"
	}
	if !json.Valid([]byte(limits)) {
		return fmt.Errorf("put plan %s: limits are not JSON", p.ID)
	}
	return mapErr(t.q.UpsertPlan(ctx, statedb.UpsertPlanParams{
		ID:         p.ID,
		Name:       p.Name,
		LimitsJson: limits,
		CreatedMs:  p.CreatedMs,
		UpdatedMs:  p.UpdatedMs,
	}))
}

// DeletePlan deletes a plan or returns ErrNotFound.
func (t *stateTx) DeletePlan(ctx context.Context, id string) error {
	n, err := t.q.DeletePlan(ctx, id)
	if err != nil {
		return mapErr(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// tenantUsage is shared by the read path and the transaction.
func tenantUsage(ctx context.Context, q *statedb.Queries, tenantID string) (store.TenantUsage, error) {
	monitors, err := q.CountTenantMonitors(ctx, tenantID)
	if err != nil {
		return store.TenantUsage{}, mapErr(err)
	}
	channels, err := q.CountTenantChannels(ctx, tenantID)
	if err != nil {
		return store.TenantUsage{}, mapErr(err)
	}
	minInterval, err := q.MinTenantMonitorInterval(ctx, tenantID)
	if err != nil {
		return store.TenantUsage{}, mapErr(err)
	}
	return store.TenantUsage{Monitors: int(monitors), Channels: int(channels), MinIntervalSeconds: int(minInterval)}, nil
}

func planFromRow(r statedb.Plan) store.Plan {
	return store.Plan{
		ID:        r.ID,
		Name:      r.Name,
		Limits:    json.RawMessage(r.LimitsJson),
		CreatedMs: r.CreatedMs,
		UpdatedMs: r.UpdatedMs,
	}
}
