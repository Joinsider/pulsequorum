// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"

	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite/statedb"
)

//go:embed migrations/state/*.sql
var stateMigrationFiles embed.FS

// StateFile is the file name of the data class A database inside the data directory.
const StateFile = "state.db"

const (
	defaultPageSize = 100
	maxPageSize     = 500
)

// StateStore implements store.State on state.db.
type StateStore struct {
	db *DB
	q  *statedb.Queries // bound to the read pool
}

var _ store.State = (*StateStore)(nil)

// OpenState opens or creates dataDir/state.db.
func OpenState(ctx context.Context, dataDir string) (*StateStore, error) {
	migrations, err := fs.Sub(stateMigrationFiles, "migrations/state")
	if err != nil {
		return nil, err
	}
	db, err := Open(ctx, Options{
		Path:        filepath.Join(dataDir, StateFile),
		Synchronous: SyncFull,
		ReadConns:   4,
		Migrations:  migrations,
	})
	if err != nil {
		return nil, err
	}
	return &StateStore{db: db, q: statedb.New(db.reader)}, nil
}

// DB exposes the underlying database for maintenance (checkpoints, snapshots).
func (s *StateStore) DB() *DB { return s.db }

// Close closes the database.
func (s *StateStore) Close() error { return s.db.Close() }

// GetTenant returns a tenant by ID.
func (s *StateStore) GetTenant(ctx context.Context, id string) (store.Tenant, error) {
	row, err := s.q.GetTenant(ctx, id)
	if err != nil {
		return store.Tenant{}, mapErr(err)
	}
	return tenantFromRow(row), nil
}

// ListTenants returns all tenants ordered by ID.
func (s *StateStore) ListTenants(ctx context.Context) ([]store.Tenant, error) {
	rows, err := s.q.ListTenants(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.Tenant, 0, len(rows))
	for _, r := range rows {
		out = append(out, tenantFromRow(r))
	}
	return out, nil
}

// GetMonitor returns a monitor of the tenant.
func (s *StateStore) GetMonitor(ctx context.Context, tenantID, id string) (store.Monitor, error) {
	row, err := s.q.GetMonitor(ctx, statedb.GetMonitorParams{TenantID: tenantID, ID: id})
	if err != nil {
		return store.Monitor{}, mapErr(err)
	}
	return s.monitorWithTags(ctx, s.q, row)
}

// ListMonitors returns one page of the tenant's monitors ordered by ID.
func (s *StateStore) ListMonitors(ctx context.Context, tenantID string, q store.MonitorQuery) (store.MonitorPage, error) {
	limit := clampLimit(q.Limit, defaultPageSize, maxPageSize)
	rows, err := s.q.ListMonitors(ctx, statedb.ListMonitorsParams{
		TenantID: tenantID,
		AfterID:  q.AfterID,
		Search:   q.Search,
		Tag:      q.Tag,
		RowLimit: int64(limit + 1),
	})
	if err != nil {
		return store.MonitorPage{}, mapErr(err)
	}

	page := store.MonitorPage{Items: make([]store.Monitor, 0, min(len(rows), limit))}
	for i, r := range rows {
		if i == limit {
			page.NextAfterID = page.Items[limit-1].ID
			break
		}
		m, err := s.monitorWithTags(ctx, s.q, r)
		if err != nil {
			return store.MonitorPage{}, err
		}
		page.Items = append(page.Items, m)
	}
	return page, nil
}

// Update runs fn in a single immediate write transaction.
func (s *StateStore) Update(ctx context.Context, fn func(tx store.StateTx) error) error {
	tx, err := s.db.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin state transaction: %w", err)
	}
	if err := fn(&stateTx{q: statedb.New(tx)}); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit state transaction: %w", err)
	}
	return nil
}

type stateTx struct {
	q *statedb.Queries
}

func (t *stateTx) PutTenant(ctx context.Context, tn store.Tenant) error {
	if tn.ID == "" {
		return errors.New("put tenant: empty ID")
	}
	limits := string(tn.Limits)
	if limits == "" {
		limits = "{}"
	}
	err := t.q.UpsertTenant(ctx, statedb.UpsertTenantParams{
		ID:           tn.ID,
		Name:         tn.Name,
		ZitadelOrgID: sql.NullString{String: tn.ZitadelOrgID, Valid: tn.ZitadelOrgID != ""},
		Plan:         tn.Plan,
		LimitsJson:   limits,
		Suspended:    boolToInt(tn.Suspended),
		CreatedMs:    tn.CreatedMs,
		UpdatedMs:    tn.UpdatedMs,
	})
	return mapErr(err)
}

func (t *stateTx) PutMonitor(ctx context.Context, m store.Monitor, expectedRevision uint64) error {
	if m.TenantID == "" || m.ID == "" {
		return errors.New("put monitor: empty tenant or monitor ID")
	}
	assignment, err := json.Marshal(m.Assignment)
	if err != nil {
		return fmt.Errorf("put monitor: assignment: %w", err)
	}
	config := string(m.Config)
	if config == "" {
		config = "{}"
	}
	newRevision, err := toInt64(expectedRevision + 1)
	if err != nil {
		return err
	}

	if expectedRevision == 0 {
		err = t.q.InsertMonitor(ctx, statedb.InsertMonitorParams{
			TenantID:          m.TenantID,
			ID:                m.ID,
			Name:              m.Name,
			Description:       m.Description,
			Type:              m.Type,
			ConfigJson:        config,
			IntervalS:         int64(m.IntervalSeconds),
			TimeoutS:          int64(m.TimeoutSeconds),
			Retries:           int64(m.Retries),
			RecoveryThreshold: int64(m.RecoveryThreshold),
			ResendIntervalMin: int64(m.ResendIntervalMinutes),
			AssignmentJson:    string(assignment),
			ConsensusPolicy:   m.ConsensusPolicy,
			Paused:            boolToInt(m.Paused),
			Revision:          newRevision,
			CreatedMs:         m.CreatedMs,
			UpdatedMs:         m.UpdatedMs,
		})
		if err != nil {
			return mapErr(err)
		}
	} else {
		expected, err := toInt64(expectedRevision)
		if err != nil {
			return err
		}
		n, err := t.q.UpdateMonitor(ctx, statedb.UpdateMonitorParams{
			Name:              m.Name,
			Description:       m.Description,
			Type:              m.Type,
			ConfigJson:        config,
			IntervalS:         int64(m.IntervalSeconds),
			TimeoutS:          int64(m.TimeoutSeconds),
			Retries:           int64(m.Retries),
			RecoveryThreshold: int64(m.RecoveryThreshold),
			ResendIntervalMin: int64(m.ResendIntervalMinutes),
			AssignmentJson:    string(assignment),
			ConsensusPolicy:   m.ConsensusPolicy,
			Paused:            boolToInt(m.Paused),
			Revision:          newRevision,
			UpdatedMs:         m.UpdatedMs,
			TenantID:          m.TenantID,
			ID:                m.ID,
			ExpectedRevision:  expected,
		})
		if err != nil {
			return mapErr(err)
		}
		if n == 0 {
			if _, err := t.q.MonitorRevision(ctx, statedb.MonitorRevisionParams{TenantID: m.TenantID, ID: m.ID}); err != nil {
				return mapErr(err)
			}
			return fmt.Errorf("monitor %s: revision is not %d: %w", m.ID, expectedRevision, store.ErrConflict)
		}
		if err := t.q.DeleteMonitorTags(ctx, statedb.DeleteMonitorTagsParams{TenantID: m.TenantID, MonitorID: m.ID}); err != nil {
			return mapErr(err)
		}
	}

	for _, tag := range normalizeTags(m.Tags) {
		if err := t.q.InsertMonitorTag(ctx, statedb.InsertMonitorTagParams{TenantID: m.TenantID, MonitorID: m.ID, Tag: tag}); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

func (t *stateTx) DeleteMonitor(ctx context.Context, tenantID, id string) error {
	n, err := t.q.DeleteMonitor(ctx, statedb.DeleteMonitorParams{TenantID: tenantID, ID: id})
	if err != nil {
		return mapErr(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *StateStore) monitorWithTags(ctx context.Context, q *statedb.Queries, r statedb.Monitor) (store.Monitor, error) {
	tags, err := q.ListMonitorTags(ctx, statedb.ListMonitorTagsParams{TenantID: r.TenantID, MonitorID: r.ID})
	if err != nil {
		return store.Monitor{}, mapErr(err)
	}
	var assignment store.Assignment
	if err := json.Unmarshal([]byte(r.AssignmentJson), &assignment); err != nil {
		return store.Monitor{}, fmt.Errorf("monitor %s: assignment: %w", r.ID, err)
	}
	return store.Monitor{
		TenantID:              r.TenantID,
		ID:                    r.ID,
		Name:                  r.Name,
		Description:           r.Description,
		Type:                  r.Type,
		Config:                json.RawMessage(r.ConfigJson),
		IntervalSeconds:       int(r.IntervalS),
		TimeoutSeconds:        int(r.TimeoutS),
		Retries:               int(r.Retries),
		RecoveryThreshold:     int(r.RecoveryThreshold),
		ResendIntervalMinutes: int(r.ResendIntervalMin),
		Assignment:            assignment,
		ConsensusPolicy:       r.ConsensusPolicy,
		Tags:                  tags,
		Paused:                r.Paused != 0,
		Revision:              uint64(max(r.Revision, 0)),
		CreatedMs:             r.CreatedMs,
		UpdatedMs:             r.UpdatedMs,
	}, nil
}

func tenantFromRow(r statedb.Tenant) store.Tenant {
	return store.Tenant{
		ID:           r.ID,
		Name:         r.Name,
		ZitadelOrgID: r.ZitadelOrgID.String,
		Plan:         r.Plan,
		Limits:       json.RawMessage(r.LimitsJson),
		Suspended:    r.Suspended != 0,
		CreatedMs:    r.CreatedMs,
		UpdatedMs:    r.UpdatedMs,
	}
}

// normalizeTags removes duplicates and empty tags and sorts them, so that every
// replica stores the same rows.
func normalizeTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t != "" {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
