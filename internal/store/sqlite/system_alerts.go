// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sqlite

import (
	"context"
	"errors"

	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite/statedb"
)

// ListSystemAlerts returns the system alerts ordered by kind and subject; with
// activeOnly only the active ones (ADR 0025).
func (s *StateStore) ListSystemAlerts(ctx context.Context, activeOnly bool) ([]store.SystemAlert, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var (
		rows []statedb.SystemAlert
		err  error
	)
	if activeOnly {
		rows, err = s.q.ListActiveSystemAlerts(ctx)
	} else {
		rows, err = s.q.ListSystemAlerts(ctx)
	}
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.SystemAlert, 0, len(rows))
	for _, r := range rows {
		out = append(out, systemAlertFromRow(r))
	}
	return out, nil
}

// ListNodeIncarnations returns the last known incarnations ordered by node ID (ADR 0025).
func (s *StateStore) ListNodeIncarnations(ctx context.Context) ([]store.NodeIncarnation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.q.ListNodeIncarnations(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.NodeIncarnation, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.NodeIncarnation{NodeID: r.NodeID, Incarnation: r.Incarnation, SeenMs: r.SeenMs})
	}
	return out, nil
}

// GetSystemAlert returns the system alert with kind and subject or ErrNotFound (ADR 0025).
func (t *stateTx) GetSystemAlert(ctx context.Context, kind, subject string) (store.SystemAlert, error) {
	r, err := t.q.GetSystemAlert(ctx, statedb.GetSystemAlertParams{Kind: kind, Subject: subject})
	if err != nil {
		return store.SystemAlert{}, mapErr(err)
	}
	return systemAlertFromRow(r), nil
}

// PutSystemAlert creates or replaces the system alert with its kind and subject.
func (t *stateTx) PutSystemAlert(ctx context.Context, a store.SystemAlert) error {
	if a.Kind == "" || a.Subject == "" {
		return errors.New("put system alert: kind and subject are required")
	}
	details := string(a.DetailsJSON)
	if details == "" {
		details = "{}"
	}
	return mapErr(t.q.UpsertSystemAlert(ctx, statedb.UpsertSystemAlertParams{
		Kind:        a.Kind,
		Subject:     a.Subject,
		Active:      boolToInt(a.Active),
		Level:       int64(max(a.Level, 0)),
		SinceMs:     a.SinceMs,
		UpdatedMs:   a.UpdatedMs,
		DetailsJson: details,
	}))
}

// PutNodeIncarnation creates or replaces the last known incarnation of a node (ADR 0025).
func (t *stateTx) PutNodeIncarnation(ctx context.Context, n store.NodeIncarnation) error {
	if n.NodeID == "" || n.Incarnation == "" {
		return errors.New("put node incarnation: node ID and incarnation are required")
	}
	return mapErr(t.q.UpsertNodeIncarnation(ctx, statedb.UpsertNodeIncarnationParams{
		NodeID: n.NodeID, Incarnation: n.Incarnation, SeenMs: n.SeenMs,
	}))
}

// Fingerprint hashes the replicated state inside the write transaction (ADR 0025).
func (t *stateTx) Fingerprint(ctx context.Context) (string, error) {
	return fingerprint(ctx, t.tx)
}

func systemAlertFromRow(r statedb.SystemAlert) store.SystemAlert {
	return store.SystemAlert{
		Kind: r.Kind, Subject: r.Subject, Active: r.Active != 0, Level: int(r.Level),
		SinceMs: r.SinceMs, UpdatedMs: r.UpdatedMs, DetailsJSON: []byte(r.DetailsJson),
	}
}
