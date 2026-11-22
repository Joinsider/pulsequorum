// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sqlite

import (
	"context"
	"errors"

	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite/statedb"
)

// ListProbeAlerts returns the private probe alerts of tenantID ordered by probe ID;
// with activeOnly only the active ones (ADR 0035).
func (s *StateStore) ListProbeAlerts(ctx context.Context, tenantID string, activeOnly bool) ([]store.ProbeAlert, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var (
		rows []statedb.ProbeAlert
		err  error
	)
	if activeOnly {
		rows, err = s.q.ListActiveTenantProbeAlerts(ctx, tenantID)
	} else {
		rows, err = s.q.ListTenantProbeAlerts(ctx, tenantID)
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return probeAlertsFromRows(rows), nil
}

// ListAllProbeAlerts returns all private probe alerts ordered by tenant ID and
// probe ID; with activeOnly only the active ones (ADR 0035).
func (s *StateStore) ListAllProbeAlerts(ctx context.Context, activeOnly bool) ([]store.ProbeAlert, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var (
		rows []statedb.ProbeAlert
		err  error
	)
	if activeOnly {
		rows, err = s.q.ListAllActiveProbeAlerts(ctx)
	} else {
		rows, err = s.q.ListAllProbeAlerts(ctx)
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return probeAlertsFromRows(rows), nil
}

// CountTenantPrivateProbes returns the number of registered private probes
// owned by tenantID (ADR 0035).
func (s *StateStore) CountTenantPrivateProbes(ctx context.Context, tenantID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, err := s.q.CountTenantPrivateProbes(ctx, tenantID)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(n), nil
}

// CountOpenTenantJoinTokens returns the number of unused, unexpired probe join
// tokens bound to tenantID at nowMs (ADR 0035).
func (s *StateStore) CountOpenTenantJoinTokens(ctx context.Context, tenantID string, nowMs int64) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, err := s.q.CountOpenTenantJoinTokens(ctx, statedb.CountOpenTenantJoinTokensParams{
		TenantID:  tenantID,
		ExpiresMs: nowMs,
	})
	if err != nil {
		return 0, mapErr(err)
	}
	return int(n), nil
}

// GetProbeAlert returns the stored private probe alert or ErrNotFound (ADR 0035).
func (t *stateTx) GetProbeAlert(ctx context.Context, tenantID, probeID string) (store.ProbeAlert, error) {
	r, err := t.q.GetProbeAlert(ctx, statedb.GetProbeAlertParams{TenantID: tenantID, ProbeID: probeID})
	if err != nil {
		return store.ProbeAlert{}, mapErr(err)
	}
	return probeAlertFromRow(r), nil
}

// PutProbeAlert creates or replaces the private probe alert (ADR 0035).
func (t *stateTx) PutProbeAlert(ctx context.Context, a store.ProbeAlert) error {
	if a.TenantID == "" || a.ProbeID == "" {
		return errors.New("put probe alert: tenant and probe ID are required")
	}
	return mapErr(t.q.UpsertProbeAlert(ctx, statedb.UpsertProbeAlertParams{
		TenantID:  a.TenantID,
		ProbeID:   a.ProbeID,
		Active:    boolToInt(a.Active),
		SinceMs:   a.SinceMs,
		UpdatedMs: a.UpdatedMs,
	}))
}

// DeleteProbeAlertsByProbe deletes any probe alert for probeID (ADR 0035).
func (t *stateTx) DeleteProbeAlertsByProbe(ctx context.Context, probeID string) error {
	return mapErr(t.q.DeleteProbeAlertsByProbe(ctx, probeID))
}

// CountTenantPrivateProbes returns the number of registered private probes
// owned by tenantID (ADR 0035).
func (t *stateTx) CountTenantPrivateProbes(ctx context.Context, tenantID string) (int, error) {
	n, err := t.q.CountTenantPrivateProbes(ctx, tenantID)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(n), nil
}

// CountOpenTenantJoinTokens returns the number of unused, unexpired probe join
// tokens bound to tenantID at nowMs (ADR 0035).
func (t *stateTx) CountOpenTenantJoinTokens(ctx context.Context, tenantID string, nowMs int64) (int, error) {
	n, err := t.q.CountOpenTenantJoinTokens(ctx, statedb.CountOpenTenantJoinTokensParams{
		TenantID:  tenantID,
		ExpiresMs: nowMs,
	})
	if err != nil {
		return 0, mapErr(err)
	}
	return int(n), nil
}

func probeAlertFromRow(r statedb.ProbeAlert) store.ProbeAlert {
	return store.ProbeAlert{
		TenantID:  r.TenantID,
		ProbeID:   r.ProbeID,
		Active:    r.Active != 0,
		SinceMs:   r.SinceMs,
		UpdatedMs: r.UpdatedMs,
	}
}

func probeAlertsFromRows(rows []statedb.ProbeAlert) []store.ProbeAlert {
	out := make([]store.ProbeAlert, 0, len(rows))
	for _, r := range rows {
		out = append(out, probeAlertFromRow(r))
	}
	return out
}
