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
	"math"
	"path/filepath"
	"slices"
	"sync"

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
//
// mu guards the database handle: RestoreSnapshot swaps the whole file while
// readers keep using the store.
type StateStore struct {
	mu   sync.RWMutex
	path string
	db   *DB
	q    *statedb.Queries // bound to the read pool
}

var _ store.State = (*StateStore)(nil)

// OpenState opens or creates dataDir/state.db.
func OpenState(ctx context.Context, dataDir string) (*StateStore, error) {
	s := &StateStore{path: filepath.Join(dataDir, StateFile)}
	if err := s.open(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *StateStore) open(ctx context.Context) error {
	migrations, err := fs.Sub(stateMigrationFiles, "migrations/state")
	if err != nil {
		return err
	}
	db, err := Open(ctx, Options{
		Path:        s.path,
		Synchronous: SyncFull,
		ReadConns:   4,
		Migrations:  migrations,
	})
	if err != nil {
		return err
	}
	s.db, s.q = db, statedb.New(db.reader)
	return nil
}

// DB exposes the underlying database for maintenance (checkpoints). The handle
// is replaced by RestoreSnapshot; callers must not keep it across a restore.
func (s *StateStore) DB() *DB {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db
}

// Close closes the database.
func (s *StateStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

// GetTenant returns a tenant by ID.
func (s *StateStore) GetTenant(ctx context.Context, id string) (store.Tenant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	row, err := s.q.GetTenant(ctx, id)
	if err != nil {
		return store.Tenant{}, mapErr(err)
	}
	return tenantFromRow(row), nil
}

// ListTenants returns all tenants ordered by ID.
func (s *StateStore) ListTenants(ctx context.Context) ([]store.Tenant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	row, err := s.q.GetMonitor(ctx, statedb.GetMonitorParams{TenantID: tenantID, ID: id})
	if err != nil {
		return store.Monitor{}, mapErr(err)
	}
	m, err := monitorWithTags(ctx, s.q, row.Monitor)
	m.State = monitorState(row.StateStatus, row.StateConfirmed, row.StateSinceMs, row.StateFlapping, row.StateFlappingSinceMs)
	return m, err
}

// ListMonitors returns one page of the tenant's monitors ordered by ID.
func (s *StateStore) ListMonitors(ctx context.Context, tenantID string, q store.MonitorQuery) (store.MonitorPage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	limit := clampLimit(q.Limit, defaultPageSize, maxPageSize)
	rows, err := s.q.ListMonitors(ctx, statedb.ListMonitorsParams{
		TenantID: tenantID,
		AfterID:  q.AfterID,
		Status:   string(q.Status),
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
		m, err := monitorWithTags(ctx, s.q, r.Monitor)
		if err != nil {
			return store.MonitorPage{}, err
		}
		m.State = monitorState(r.StateStatus, r.StateConfirmed, r.StateSinceMs, r.StateFlapping, r.StateFlappingSinceMs)
		page.Items = append(page.Items, m)
	}
	return page, nil
}

// GetIncident returns an incident of the tenant.
func (s *StateStore) GetIncident(ctx context.Context, tenantID, id string) (store.Incident, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	row, err := s.q.GetIncident(ctx, statedb.GetIncidentParams{TenantID: tenantID, ID: id})
	if err != nil {
		return store.Incident{}, mapErr(err)
	}
	return incidentFromRow(row.Incident, row.MonitorName)
}

// ListIncidents returns one page of the tenant's incidents, newest first.
func (s *StateStore) ListIncidents(ctx context.Context, tenantID string, q store.IncidentQuery) (store.IncidentPage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	limit := clampLimit(q.Limit, defaultPageSize, maxPageSize)
	toMs := q.ToMs
	if toMs == 0 {
		toMs = math.MaxInt64
	}
	before := store.IncidentCursor{StartedMs: math.MaxInt64}
	if q.After != nil {
		before = *q.After
	}
	rows, err := s.q.ListIncidents(ctx, statedb.ListIncidentsParams{
		TenantID:        tenantID,
		MonitorID:       q.MonitorID,
		ToMs:            toMs,
		FromMs:          q.FromMs,
		OpenOnly:        boolToInt(q.OpenOnly),
		BeforeStartedMs: before.StartedMs,
		BeforeID:        before.ID,
		RowLimit:        int64(limit + 1),
	})
	if err != nil {
		return store.IncidentPage{}, mapErr(err)
	}
	page := store.IncidentPage{Items: make([]store.Incident, 0, min(len(rows), limit))}
	for i, r := range rows {
		if i == limit {
			last := page.Items[limit-1]
			page.Next = &store.IncidentCursor{StartedMs: last.StartedMs, ID: last.ID}
			break
		}
		inc, err := incidentFromRow(r.Incident, r.MonitorName)
		if err != nil {
			return store.IncidentPage{}, err
		}
		page.Items = append(page.Items, inc)
	}
	return page, nil
}

// StatusHistory returns the status changes in [fromMs, toMs) and the last one before.
func (s *StateStore) StatusHistory(ctx context.Context, tenantID, monitorID string, fromMs, toMs int64) ([]store.StatusChange, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.q.StatusHistory(ctx, statedb.StatusHistoryParams{TenantID: tenantID, MonitorID: monitorID, FromMs: fromMs, ToMs: toMs})
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.StatusChange, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.StatusChange{SinceMs: r.SinceMs, Status: store.MonitorStatus(r.Status)})
	}
	return out, nil
}

// StatusHistoryBatch returns StatusHistory for each listed monitor of the tenant.
func (s *StateStore) StatusHistoryBatch(ctx context.Context, tenantID string, monitorIDs []string, fromMs, toMs int64) (map[string][]store.StatusChange, error) {
	out := make(map[string][]store.StatusChange, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return out, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.q.StatusHistoryBatch(ctx, statedb.StatusHistoryBatchParams{TenantID: tenantID, MonitorIds: monitorIDs, FromMs: fromMs, ToMs: toMs})
	if err != nil {
		return nil, mapErr(err)
	}
	for _, r := range rows {
		out[r.MonitorID] = append(out[r.MonitorID], store.StatusChange{SinceMs: r.SinceMs, Status: store.MonitorStatus(r.Status)})
	}
	return out, nil
}

// ListSecretKeys returns all data encryption keys ordered by creation.
func (s *StateStore) ListSecretKeys(ctx context.Context) ([]store.SecretKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.q.ListSecretKeys(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.SecretKey, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.SecretKey{
			KeyID: r.KeyID, WrappedDEK: r.WrappedDek, KEKFingerprint: r.KekFingerprint,
			CreatedMs: r.CreatedMs, RetiredMs: r.RetiredMs,
		})
	}
	return out, nil
}

// Update runs fn in a single immediate write transaction.
func (s *StateStore) Update(ctx context.Context, fn func(tx store.StateTx) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
	secretsJSON, err := json.Marshal(nonNilMap(m.Secrets))
	if err != nil {
		return fmt.Errorf("put monitor: secrets: %w", err)
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
			SecretsJson:       string(secretsJSON),
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
			SecretsJson:       string(secretsJSON),
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
		if err := t.q.DeleteMonitorChannels(ctx, statedb.DeleteMonitorChannelsParams{TenantID: m.TenantID, MonitorID: m.ID}); err != nil {
			return mapErr(err)
		}
	}
	for _, ch := range normalizeTags(m.ChannelIDs) {
		if err := t.q.InsertMonitorChannel(ctx, statedb.InsertMonitorChannelParams{TenantID: m.TenantID, MonitorID: m.ID, ChannelID: ch}); err != nil {
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

func (t *stateTx) PutSecretKey(ctx context.Context, k store.SecretKey) error {
	if k.KeyID == "" || len(k.WrappedDEK) == 0 || k.KEKFingerprint == "" {
		return errors.New("put secret key: key ID, wrapped DEK and KEK fingerprint are required")
	}
	return mapErr(t.q.UpsertSecretKey(ctx, statedb.UpsertSecretKeyParams{
		KeyID:          k.KeyID,
		WrappedDek:     k.WrappedDEK,
		KekFingerprint: k.KEKFingerprint,
		CreatedMs:      k.CreatedMs,
		RetiredMs:      k.RetiredMs,
	}))
}

func (t *stateTx) SetAppliedIndex(ctx context.Context, index uint64) error {
	v, err := toInt64(index)
	if err != nil {
		return err
	}
	return mapErr(t.q.SetFSMMeta(ctx, statedb.SetFSMMetaParams{Key: metaAppliedIndex, Value: v}))
}

func (t *stateTx) CommandLevel(ctx context.Context) (uint32, error) {
	return commandLevel(ctx, t.q)
}

func (t *stateTx) SetCommandLevel(ctx context.Context, level uint32) error {
	return mapErr(t.q.SetFSMMeta(ctx, statedb.SetFSMMetaParams{Key: metaCommandLevel, Value: int64(level)}))
}

func (t *stateTx) GetDedup(ctx context.Context, requestID string) (store.DedupEntry, error) {
	r, err := t.q.GetDedup(ctx, requestID)
	if err != nil {
		return store.DedupEntry{}, mapErr(err)
	}
	return store.DedupEntry{
		RequestID:    r.RequestID,
		AppliedIndex: uint64(max(r.AppliedIndex, 0)),
		IssuedAtMs:   r.IssuedAtMs,
		Result:       json.RawMessage(r.ResultJson),
		ErrorCode:    r.ErrorCode,
		ErrorMessage: r.ErrorMessage,
	}, nil
}

func (t *stateTx) PutDedup(ctx context.Context, e store.DedupEntry) error {
	idx, err := toInt64(e.AppliedIndex)
	if err != nil {
		return err
	}
	return mapErr(t.q.InsertDedup(ctx, statedb.InsertDedupParams{
		RequestID:    e.RequestID,
		AppliedIndex: idx,
		IssuedAtMs:   e.IssuedAtMs,
		ResultJson:   string(e.Result),
		ErrorCode:    e.ErrorCode,
		ErrorMessage: e.ErrorMessage,
	}))
}

func (t *stateTx) PruneDedup(ctx context.Context, beforeMs int64) error {
	_, err := t.q.PruneDedup(ctx, beforeMs)
	return mapErr(err)
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

func (t *stateTx) GetTenant(ctx context.Context, id string) (store.Tenant, error) {
	row, err := t.q.GetTenant(ctx, id)
	if err != nil {
		return store.Tenant{}, mapErr(err)
	}
	return tenantFromRow(row), nil
}

func (t *stateTx) GetMonitorState(ctx context.Context, tenantID, monitorID string) (store.MonitorState, error) {
	r, err := t.q.GetMonitorState(ctx, statedb.GetMonitorStateParams{TenantID: tenantID, MonitorID: monitorID})
	if err != nil {
		return store.MonitorState{}, mapErr(err)
	}
	return monitorState(r.Status, r.Confirmed, r.SinceMs, r.Flapping, r.FlappingSinceMs), nil
}

func (t *stateTx) SetMonitorFlapping(ctx context.Context, tenantID, monitorID string, flapping bool, sinceMs int64) error {
	n, err := t.q.SetMonitorFlapping(ctx, statedb.SetMonitorFlappingParams{
		Flapping: boolToInt(flapping), FlappingSinceMs: sinceMs, TenantID: tenantID, MonitorID: monitorID,
	})
	if err != nil {
		return mapErr(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (t *stateTx) StatusChangesSince(ctx context.Context, tenantID, monitorID string, fromMs int64) ([]store.StatusChange, error) {
	rows, err := t.q.StatusChangesSince(ctx, statedb.StatusChangesSinceParams{TenantID: tenantID, MonitorID: monitorID, FromMs: fromMs})
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.StatusChange, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.StatusChange{SinceMs: r.SinceMs, Status: store.MonitorStatus(r.Status)})
	}
	return out, nil
}

func (t *stateTx) PutMonitorState(ctx context.Context, tenantID, monitorID string, st store.MonitorState) error {
	if !st.Status.Valid() {
		return fmt.Errorf("put monitor state: invalid status %q", st.Status)
	}
	err := t.q.UpsertMonitorState(ctx, statedb.UpsertMonitorStateParams{
		TenantID:  tenantID,
		MonitorID: monitorID,
		Status:    string(st.Status),
		Confirmed: boolToInt(st.Confirmed),
		SinceMs:   st.SinceMs,
	})
	if err != nil {
		return mapErr(err)
	}
	return mapErr(t.q.UpsertStatusHistory(ctx, statedb.UpsertStatusHistoryParams{
		TenantID: tenantID, MonitorID: monitorID, SinceMs: st.SinceMs, Status: string(st.Status),
	}))
}

func (t *stateTx) PruneStatusHistory(ctx context.Context, tenantID, monitorID string, beforeMs int64) error {
	return mapErr(t.q.PruneStatusHistory(ctx, statedb.PruneStatusHistoryParams{TenantID: tenantID, MonitorID: monitorID, BeforeMs: beforeMs}))
}

func (t *stateTx) OpenIncident(ctx context.Context, inc store.Incident) error {
	if inc.TenantID == "" || inc.ID == "" || inc.MonitorID == "" {
		return errors.New("open incident: empty tenant, incident or monitor ID")
	}
	nodes := inc.AffectedNodes
	if nodes == nil {
		nodes = []string{}
	}
	affected, err := json.Marshal(nodes)
	if err != nil {
		return fmt.Errorf("open incident: affected nodes: %w", err)
	}
	return mapErr(t.q.InsertIncident(ctx, statedb.InsertIncidentParams{
		TenantID:          inc.TenantID,
		ID:                inc.ID,
		MonitorID:         inc.MonitorID,
		StartedMs:         inc.StartedMs,
		Cause:             inc.Cause,
		ErrorClass:        inc.ErrorClass,
		AffectedNodesJson: string(affected),
		Confirmed:         boolToInt(inc.Confirmed),
	}))
}

func (t *stateTx) ResolveIncident(ctx context.Context, tenantID, monitorID string, resolvedMs int64) error {
	_, err := t.q.ResolveIncident(ctx, statedb.ResolveIncidentParams{ResolvedMs: resolvedMs, TenantID: tenantID, MonitorID: monitorID})
	return mapErr(err)
}

func monitorState(status string, confirmed, sinceMs, flapping, flappingSinceMs int64) store.MonitorState {
	return store.MonitorState{
		Status: store.MonitorStatus(status), Confirmed: confirmed != 0, SinceMs: sinceMs,
		Flapping: flapping != 0, FlappingSinceMs: flappingSinceMs,
	}
}

func incidentFromRow(r statedb.Incident, monitorName string) (store.Incident, error) {
	var nodes []string
	if err := json.Unmarshal([]byte(r.AffectedNodesJson), &nodes); err != nil {
		return store.Incident{}, fmt.Errorf("incident %s: affected nodes: %w", r.ID, err)
	}
	return store.Incident{
		TenantID:      r.TenantID,
		ID:            r.ID,
		MonitorID:     r.MonitorID,
		MonitorName:   monitorName,
		StartedMs:     r.StartedMs,
		ResolvedMs:    r.ResolvedMs,
		Cause:         r.Cause,
		ErrorClass:    r.ErrorClass,
		AffectedNodes: nodes,
		Confirmed:     r.Confirmed != 0,
	}, nil
}

func monitorWithTags(ctx context.Context, q *statedb.Queries, r statedb.Monitor) (store.Monitor, error) {
	tags, err := q.ListMonitorTags(ctx, statedb.ListMonitorTagsParams{TenantID: r.TenantID, MonitorID: r.ID})
	if err != nil {
		return store.Monitor{}, mapErr(err)
	}
	channels, err := q.ListMonitorChannelIDs(ctx, statedb.ListMonitorChannelIDsParams{TenantID: r.TenantID, MonitorID: r.ID})
	if err != nil {
		return store.Monitor{}, mapErr(err)
	}
	var assignment store.Assignment
	if err := json.Unmarshal([]byte(r.AssignmentJson), &assignment); err != nil {
		return store.Monitor{}, fmt.Errorf("monitor %s: assignment: %w", r.ID, err)
	}
	var secrets map[string]string
	if err := json.Unmarshal([]byte(r.SecretsJson), &secrets); err != nil {
		return store.Monitor{}, fmt.Errorf("monitor %s: secrets: %w", r.ID, err)
	}
	if len(secrets) == 0 {
		secrets = nil
	}
	return store.Monitor{
		TenantID:              r.TenantID,
		ID:                    r.ID,
		Name:                  r.Name,
		Description:           r.Description,
		Type:                  r.Type,
		Config:                json.RawMessage(r.ConfigJson),
		Secrets:               secrets,
		IntervalSeconds:       int(r.IntervalS),
		TimeoutSeconds:        int(r.TimeoutS),
		Retries:               int(r.Retries),
		RecoveryThreshold:     int(r.RecoveryThreshold),
		ResendIntervalMinutes: int(r.ResendIntervalMin),
		Assignment:            assignment,
		ConsensusPolicy:       r.ConsensusPolicy,
		Tags:                  tags,
		ChannelIDs:            channels,
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

// nonNilMap returns m, or an empty map for nil, so that it marshals to "{}".
func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
