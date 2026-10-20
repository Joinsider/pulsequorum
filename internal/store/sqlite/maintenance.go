// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite/statedb"
)

// GetMaintenanceWindow returns a maintenance window of the tenant.
func (s *StateStore) GetMaintenanceWindow(ctx context.Context, tenantID, id string) (store.MaintenanceWindow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return getMaintenanceWindow(ctx, s.q, tenantID, id)
}

// ListMaintenanceWindows returns the tenant's windows ordered by name and ID.
func (s *StateStore) ListMaintenanceWindows(ctx context.Context, tenantID string) ([]store.MaintenanceWindow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.q.ListMaintenanceWindows(ctx, tenantID)
	if err != nil {
		return nil, mapErr(err)
	}
	return maintenanceWindowsFromRows(rows)
}

// ListAllMaintenanceWindows returns the windows of all tenants ordered by tenant ID and ID.
func (s *StateStore) ListAllMaintenanceWindows(ctx context.Context) ([]store.MaintenanceWindow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.q.ListAllMaintenanceWindows(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	return maintenanceWindowsFromRows(rows)
}

func (t *stateTx) PutMaintenanceWindow(ctx context.Context, w store.MaintenanceWindow, expectedRevision uint64) error {
	if w.TenantID == "" || w.ID == "" {
		return errors.New("put maintenance window: empty tenant or window ID")
	}
	// Same normalization as tags: sorted, unique, no empty IDs. Replicas must
	// store identical rows.
	monitorIDs := normalizeTags(w.MonitorIDs)
	monitorIDsJSON, err := json.Marshal(monitorIDs)
	if err != nil {
		return fmt.Errorf("put maintenance window: monitor IDs: %w", err)
	}
	scheduleJSON, err := json.Marshal(w.Schedule)
	if err != nil {
		return fmt.Errorf("put maintenance window: schedule: %w", err)
	}
	revision, err := toInt64(expectedRevision + 1)
	if err != nil {
		return err
	}
	if expectedRevision == 0 {
		return mapErr(t.q.InsertMaintenanceWindow(ctx, statedb.InsertMaintenanceWindowParams{
			TenantID: w.TenantID, ID: w.ID, Name: w.Name, Description: w.Description,
			MonitorIdsJson: string(monitorIDsJSON), ScheduleJson: string(scheduleJSON), Active: boolToInt(w.Active),
			Revision: revision, CreatedMs: w.CreatedMs, UpdatedMs: w.UpdatedMs,
		}))
	}
	expected, err := toInt64(expectedRevision)
	if err != nil {
		return err
	}
	n, err := t.q.UpdateMaintenanceWindow(ctx, statedb.UpdateMaintenanceWindowParams{
		Name: w.Name, Description: w.Description, MonitorIdsJson: string(monitorIDsJSON),
		ScheduleJson: string(scheduleJSON), Active: boolToInt(w.Active), Revision: revision, UpdatedMs: w.UpdatedMs,
		TenantID: w.TenantID, ID: w.ID, ExpectedRevision: expected,
	})
	if err != nil {
		return mapErr(err)
	}
	if n == 0 {
		if _, err := t.q.MaintenanceWindowRevision(ctx, statedb.MaintenanceWindowRevisionParams{TenantID: w.TenantID, ID: w.ID}); err != nil {
			return mapErr(err)
		}
		return fmt.Errorf("maintenance window %s: revision is not %d: %w", w.ID, expectedRevision, store.ErrConflict)
	}
	return nil
}

func (t *stateTx) DeleteMaintenanceWindow(ctx context.Context, tenantID, id string) error {
	n, err := t.q.DeleteMaintenanceWindow(ctx, statedb.DeleteMaintenanceWindowParams{TenantID: tenantID, ID: id})
	if err != nil {
		return mapErr(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func getMaintenanceWindow(ctx context.Context, q *statedb.Queries, tenantID, id string) (store.MaintenanceWindow, error) {
	r, err := q.GetMaintenanceWindow(ctx, statedb.GetMaintenanceWindowParams{TenantID: tenantID, ID: id})
	if err != nil {
		return store.MaintenanceWindow{}, mapErr(err)
	}
	return maintenanceWindowFromRow(r)
}

func maintenanceWindowsFromRows(rows []statedb.MaintenanceWindow) ([]store.MaintenanceWindow, error) {
	out := make([]store.MaintenanceWindow, 0, len(rows))
	for _, r := range rows {
		w, err := maintenanceWindowFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

func maintenanceWindowFromRow(r statedb.MaintenanceWindow) (store.MaintenanceWindow, error) {
	w := store.MaintenanceWindow{
		TenantID: r.TenantID, ID: r.ID, Name: r.Name, Description: r.Description,
		Active: r.Active != 0, Revision: uint64(max(r.Revision, 0)), CreatedMs: r.CreatedMs, UpdatedMs: r.UpdatedMs,
	}
	if err := json.Unmarshal([]byte(r.MonitorIdsJson), &w.MonitorIDs); err != nil {
		return store.MaintenanceWindow{}, fmt.Errorf("maintenance window %s: monitor IDs: %w", r.ID, err)
	}
	if err := json.Unmarshal([]byte(r.ScheduleJson), &w.Schedule); err != nil {
		return store.MaintenanceWindow{}, fmt.Errorf("maintenance window %s: schedule: %w", r.ID, err)
	}
	return w, nil
}
