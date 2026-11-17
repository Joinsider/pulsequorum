// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite/metricsdb"
	"github.com/joinsider/uptimecluster/internal/store/sqlite/statedb"
)

// metaLastIssuedAt is the fsm_meta key of the command time of the last applied
// command (ADR 0024).
const metaLastIssuedAt = "last_issued_at_ms"

// validVerdict reports whether verdict is one of the Verdict constants.
func validVerdict(verdict string) bool {
	switch verdict {
	case store.VerdictConfirmed, store.VerdictFalseAlarm, store.VerdictUnverifiable:
		return true
	}
	return false
}

// encodeReporters returns the JSON array stored for the reporters.
func encodeReporters(reporters []string) (string, error) {
	if reporters == nil {
		reporters = []string{}
	}
	b, err := json.Marshal(reporters)
	if err != nil {
		return "", fmt.Errorf("reporters: %w", err)
	}
	return string(b), nil
}

// decodeReporters parses the JSON array of the reporters; it never returns nil.
func decodeReporters(raw string) ([]string, error) {
	out := []string{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("reporters: %w", err)
	}
	return out, nil
}

// lastIssuedAt reads the command time of the last applied command, 0 if unset.
func lastIssuedAt(ctx context.Context, q interface {
	GetFSMMeta(ctx context.Context, key string) (int64, error)
}) (int64, error) {
	v, err := q.GetFSMMeta(ctx, metaLastIssuedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read last issued time: %w", err)
	}
	return v, nil
}

// LastIssuedAt returns the command time of the last applied command (ADR 0024).
func (s *StateStore) LastIssuedAt(ctx context.Context) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return lastIssuedAt(ctx, s.q)
}

// ListOpenUnconfirmedAlerts returns up to limit alerts without verdict (ADR 0024).
func (s *StateStore) ListOpenUnconfirmedAlerts(ctx context.Context, limit int) ([]store.UnconfirmedAlert, error) {
	if limit <= 0 {
		return nil, errors.New("list unconfirmed alerts: limit must be positive")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.q.ListOpenUnconfirmedAlerts(ctx, int64(limit))
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.UnconfirmedAlert, 0, len(rows))
	for _, r := range rows {
		a, err := unconfirmedAlertFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func unconfirmedAlertFromRow(r statedb.UnconfirmedAlert) (store.UnconfirmedAlert, error) {
	reporters, err := decodeReporters(r.Reporters)
	if err != nil {
		return store.UnconfirmedAlert{}, fmt.Errorf("unconfirmed alert %s/%d: %w", r.NodeID, r.LocalID, err)
	}
	return store.UnconfirmedAlert{
		NodeID:     r.NodeID,
		LocalID:    r.LocalID,
		TenantID:   r.TenantID,
		MonitorID:  r.MonitorID,
		ChannelID:  r.ChannelID,
		Status:     r.Status,
		SinceMs:    r.SinceMs,
		SentMs:     r.SentMs,
		Outcome:    r.Outcome,
		Reachable:  int(r.Reachable),
		Total:      int(r.Total),
		Reporters:  reporters,
		ReportedMs: r.ReportedMs,
		Verdict:    r.Verdict,
		ResolvedMs: r.ResolvedMs,
	}, nil
}

// SetLastIssuedAt stores the command time of the last applied command (ADR 0024).
func (t *stateTx) SetLastIssuedAt(ctx context.Context, ms int64) error {
	return mapErr(t.q.SetFSMMeta(ctx, statedb.SetFSMMetaParams{Key: metaLastIssuedAt, Value: ms}))
}

// LastIssuedAt returns the command time of the last applied command, 0 if none.
func (t *stateTx) LastIssuedAt(ctx context.Context) (int64, error) {
	return lastIssuedAt(ctx, t.q)
}

// ReplaceStatusHistory replaces the changes of a monitor in [fromMs, toMs) (FA-UPT-07).
func (t *stateTx) ReplaceStatusHistory(ctx context.Context, tenantID, monitorID string, fromMs, toMs int64, entries []store.StatusChange) error {
	if fromMs >= toMs {
		return fmt.Errorf("replace status history: empty interval [%d, %d)", fromMs, toMs)
	}
	for _, e := range entries {
		if e.SinceMs < fromMs || e.SinceMs >= toMs {
			return fmt.Errorf("replace status history: change at %d outside [%d, %d)", e.SinceMs, fromMs, toMs)
		}
	}
	if err := t.q.DeleteStatusHistoryBetween(ctx, statedb.DeleteStatusHistoryBetweenParams{
		TenantID: tenantID, MonitorID: monitorID, FromMs: fromMs, ToMs: toMs,
	}); err != nil {
		return mapErr(err)
	}
	for _, e := range entries {
		if err := t.q.UpsertStatusHistory(ctx, statedb.UpsertStatusHistoryParams{
			TenantID: tenantID, MonitorID: monitorID, SinceMs: e.SinceMs, Status: string(e.Status),
		}); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

// InsertUnconfirmedAlerts stores reported alerts; known ones are left unchanged.
func (t *stateTx) InsertUnconfirmedAlerts(ctx context.Context, alerts []store.UnconfirmedAlert) error {
	for _, a := range alerts {
		if a.NodeID == "" || a.LocalID <= 0 {
			return errors.New("insert unconfirmed alert: empty node ID or local ID")
		}
		reporters, err := encodeReporters(a.Reporters)
		if err != nil {
			return fmt.Errorf("insert unconfirmed alert %s/%d: %w", a.NodeID, a.LocalID, err)
		}
		if err := t.q.InsertUnconfirmedAlert(ctx, statedb.InsertUnconfirmedAlertParams{
			NodeID:     a.NodeID,
			LocalID:    a.LocalID,
			TenantID:   a.TenantID,
			MonitorID:  a.MonitorID,
			ChannelID:  a.ChannelID,
			Status:     a.Status,
			SinceMs:    a.SinceMs,
			SentMs:     a.SentMs,
			Outcome:    a.Outcome,
			Reachable:  int64(a.Reachable),
			Total:      int64(a.Total),
			Reporters:  reporters,
			ReportedMs: a.ReportedMs,
		}); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

// GetUnconfirmedAlert returns a stored alert or ErrNotFound.
func (t *stateTx) GetUnconfirmedAlert(ctx context.Context, nodeID string, localID int64) (store.UnconfirmedAlert, error) {
	r, err := t.q.GetUnconfirmedAlert(ctx, statedb.GetUnconfirmedAlertParams{NodeID: nodeID, LocalID: localID})
	if err != nil {
		return store.UnconfirmedAlert{}, mapErr(err)
	}
	return unconfirmedAlertFromRow(r)
}

// ResolveUnconfirmedAlert stores the verdict of an open alert.
func (t *stateTx) ResolveUnconfirmedAlert(ctx context.Context, nodeID string, localID int64, verdict string, resolvedMs int64) error {
	if !validVerdict(verdict) {
		return fmt.Errorf("resolve unconfirmed alert: unknown verdict %q", verdict)
	}
	n, err := t.q.ResolveUnconfirmedAlert(ctx, statedb.ResolveUnconfirmedAlertParams{
		Verdict: verdict, ResolvedMs: resolvedMs, NodeID: nodeID, LocalID: localID,
	})
	if err != nil {
		return mapErr(err)
	}
	if n == 0 {
		if _, err := t.GetUnconfirmedAlert(ctx, nodeID, localID); err != nil {
			return err
		}
		return fmt.Errorf("resolve unconfirmed alert %s/%d: %w", nodeID, localID, store.ErrConflict)
	}
	return nil
}

// InsertDegradedAlert appends an alert to the node-local degraded log.
func (m *MetricsStore) InsertDegradedAlert(ctx context.Context, a store.DegradedAlert) (int64, error) {
	reporters, err := encodeReporters(a.Reporters)
	if err != nil {
		return 0, fmt.Errorf("insert degraded alert: %w", err)
	}
	id, err := metricsdb.New(m.db.writer).InsertDegradedAlert(ctx, metricsdb.InsertDegradedAlertParams{
		TenantID:  a.TenantID,
		MonitorID: a.MonitorID,
		ChannelID: a.ChannelID,
		Status:    a.Status,
		SinceMs:   a.SinceMs,
		SentMs:    a.SentMs,
		Outcome:   a.Outcome,
		Reachable: int64(a.Reachable),
		Total:     int64(a.Total),
		Reporters: reporters,
	})
	if err != nil {
		return 0, fmt.Errorf("insert degraded alert: %w", mapErr(err))
	}
	return id, nil
}

// ListUnreportedDegradedAlerts returns up to limit alerts not yet reported, by ID.
func (m *MetricsStore) ListUnreportedDegradedAlerts(ctx context.Context, limit int) ([]store.DegradedAlert, error) {
	if limit <= 0 {
		return nil, errors.New("list degraded alerts: limit must be positive")
	}
	rows, err := m.q.ListUnreportedDegradedAlerts(ctx, int64(limit))
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.DegradedAlert, 0, len(rows))
	for _, r := range rows {
		reporters, err := decodeReporters(r.Reporters)
		if err != nil {
			return nil, fmt.Errorf("degraded alert %d: %w", r.ID, err)
		}
		out = append(out, store.DegradedAlert{
			ID:        r.ID,
			TenantID:  r.TenantID,
			MonitorID: r.MonitorID,
			ChannelID: r.ChannelID,
			Status:    r.Status,
			SinceMs:   r.SinceMs,
			SentMs:    r.SentMs,
			Outcome:   r.Outcome,
			Reachable: int(r.Reachable),
			Total:     int(r.Total),
			Reporters: reporters,
		})
	}
	return out, nil
}

// MarkDegradedAlertsReported records in one transaction that the alerts reached the leader.
func (m *MetricsStore) MarkDegradedAlertsReported(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := m.db.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mark degraded alerts: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := metricsdb.New(tx)
	for _, id := range slices.Compact(slices.Sorted(slices.Values(ids))) {
		if err := q.MarkDegradedAlertReported(ctx, id); err != nil {
			return fmt.Errorf("mark degraded alert %d: %w", id, mapErr(err))
		}
	}
	return mapErr(tx.Commit())
}

// PruneDegradedAlerts deletes the alerts sent before beforeMs.
func (m *MetricsStore) PruneDegradedAlerts(ctx context.Context, beforeMs int64) (int64, error) {
	n, err := metricsdb.New(m.db.writer).PruneDegradedAlerts(ctx, beforeMs)
	if err != nil {
		return 0, fmt.Errorf("prune degraded alerts: %w", mapErr(err))
	}
	return n, nil
}
