// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite/statedb"
)

// GetChannel returns a notification channel of the tenant.
func (s *StateStore) GetChannel(ctx context.Context, tenantID, id string) (store.NotificationChannel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return getChannel(ctx, s.q, tenantID, id)
}

// ListChannels returns the tenant's channels ordered by name.
func (s *StateStore) ListChannels(ctx context.Context, tenantID string) ([]store.NotificationChannel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.q.ListChannels(ctx, tenantID)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.NotificationChannel, 0, len(rows))
	for _, r := range rows {
		c, err := channelFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// DueAlerts returns pending deliveries due at nowMs.
func (s *StateStore) DueAlerts(ctx context.Context, nowMs int64, limit int) ([]store.AlertDelivery, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.q.DueAlerts(ctx, statedb.DueAlertsParams{NowMs: nowMs, RowLimit: int64(clampLimit(limit, defaultPageSize, maxPageSize))})
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.AlertDelivery, 0, len(rows))
	for _, r := range rows {
		out = append(out, alertFromRow(r))
	}
	return out, nil
}

// GetAlert returns one outbox entry.
func (s *StateStore) GetAlert(ctx context.Context, alertKey, channelID string) (store.AlertDelivery, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return getAlert(ctx, s.q, alertKey, channelID)
}

func (t *stateTx) GetMonitor(ctx context.Context, tenantID, id string) (store.Monitor, error) {
	row, err := t.q.GetMonitor(ctx, statedb.GetMonitorParams{TenantID: tenantID, ID: id})
	if err != nil {
		return store.Monitor{}, mapErr(err)
	}
	m, err := monitorWithTags(ctx, t.q, row.Monitor)
	m.State = monitorState(row.StateStatus, row.StateConfirmed, row.StateSinceMs, row.StateFlapping, row.StateFlappingSinceMs)
	return m, err
}

func (t *stateTx) PutChannel(ctx context.Context, c store.NotificationChannel, expectedRevision uint64) error {
	if c.TenantID == "" || c.ID == "" {
		return errors.New("put channel: empty tenant or channel ID")
	}
	headers := c.WebhookHeaders
	if headers == nil {
		headers = map[string]string{}
	}
	headersJSON, err := json.Marshal(headers) // map keys are sorted: deterministic
	if err != nil {
		return fmt.Errorf("put channel: headers: %w", err)
	}
	events := normalizeTags(c.Events)
	eventsJSON, err := json.Marshal(events)
	if err != nil {
		return fmt.Errorf("put channel: events: %w", err)
	}
	revision, err := toInt64(expectedRevision + 1)
	if err != nil {
		return err
	}
	if expectedRevision == 0 {
		return mapErr(t.q.InsertChannel(ctx, statedb.InsertChannelParams{
			TenantID: c.TenantID, ID: c.ID, Name: c.Name, Kind: c.Kind, UrlEnc: c.URLEnc,
			WebhookMethod: c.WebhookMethod, WebhookHeadersJson: string(headersJSON), WebhookTemplate: c.WebhookTemplate,
			EventsJson: string(eventsJSON), IsDefault: boolToInt(c.IsDefault), Revision: revision,
			CreatedMs: c.CreatedMs, UpdatedMs: c.UpdatedMs,
		}))
	}
	expected, err := toInt64(expectedRevision)
	if err != nil {
		return err
	}
	n, err := t.q.UpdateChannel(ctx, statedb.UpdateChannelParams{
		Name: c.Name, Kind: c.Kind, UrlEnc: c.URLEnc, WebhookMethod: c.WebhookMethod,
		WebhookHeadersJson: string(headersJSON), WebhookTemplate: c.WebhookTemplate, EventsJson: string(eventsJSON),
		IsDefault: boolToInt(c.IsDefault), Revision: revision, UpdatedMs: c.UpdatedMs,
		TenantID: c.TenantID, ID: c.ID, ExpectedRevision: expected,
	})
	if err != nil {
		return mapErr(err)
	}
	if n == 0 {
		if _, err := t.q.ChannelRevision(ctx, statedb.ChannelRevisionParams{TenantID: c.TenantID, ID: c.ID}); err != nil {
			return mapErr(err)
		}
		return fmt.Errorf("channel %s: revision is not %d: %w", c.ID, expectedRevision, store.ErrConflict)
	}
	return nil
}

func (t *stateTx) DeleteChannel(ctx context.Context, tenantID, id string) error {
	n, err := t.q.DeleteChannel(ctx, statedb.DeleteChannelParams{TenantID: tenantID, ID: id})
	if err != nil {
		return mapErr(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (t *stateTx) GetChannel(ctx context.Context, tenantID, id string) (store.NotificationChannel, error) {
	return getChannel(ctx, t.q, tenantID, id)
}

func (t *stateTx) ListChannels(ctx context.Context, tenantID string) ([]store.NotificationChannel, error) {
	rows, err := t.q.ListChannels(ctx, tenantID)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.NotificationChannel, 0, len(rows))
	for _, r := range rows {
		c, err := channelFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (t *stateTx) SetChannelState(ctx context.Context, tenantID, id, state, lastError string) error {
	return mapErr(t.q.SetChannelState(ctx, statedb.SetChannelStateParams{State: state, LastError: lastError, TenantID: tenantID, ID: id}))
}

func (t *stateTx) EnqueueAlert(ctx context.Context, a store.AlertDelivery) error {
	return mapErr(t.q.InsertAlert(ctx, statedb.InsertAlertParams{
		TenantID: a.TenantID, AlertKey: a.AlertKey, ChannelID: a.ChannelID,
		// A system alert has no monitor: NULL, so the monitor foreign key is not checked (ADR 0025).
		MonitorID:   sql.NullString{String: a.MonitorID, Valid: a.MonitorID != ""},
		PayloadJson: string(a.Payload), NextAttemptMs: a.NextAttemptMs, CreatedMs: a.CreatedMs, UpdatedMs: a.UpdatedMs,
	}))
}

func (t *stateTx) GetAlert(ctx context.Context, alertKey, channelID string) (store.AlertDelivery, error) {
	return getAlert(ctx, t.q, alertKey, channelID)
}

func (t *stateTx) UpdateAlert(ctx context.Context, a store.AlertDelivery) error {
	return mapErr(t.q.UpdateAlert(ctx, statedb.UpdateAlertParams{
		Attempts: int64(a.Attempts), NextAttemptMs: a.NextAttemptMs, State: a.State, LastError: a.LastError,
		UpdatedMs: a.UpdatedMs, AlertKey: a.AlertKey, ChannelID: a.ChannelID,
	}))
}

func (t *stateTx) PruneAlerts(ctx context.Context, beforeMs int64) error {
	return mapErr(t.q.PruneAlerts(ctx, beforeMs))
}

func getChannel(ctx context.Context, q *statedb.Queries, tenantID, id string) (store.NotificationChannel, error) {
	r, err := q.GetChannel(ctx, statedb.GetChannelParams{TenantID: tenantID, ID: id})
	if err != nil {
		return store.NotificationChannel{}, mapErr(err)
	}
	return channelFromRow(r)
}

func getAlert(ctx context.Context, q *statedb.Queries, alertKey, channelID string) (store.AlertDelivery, error) {
	r, err := q.GetAlert(ctx, statedb.GetAlertParams{AlertKey: alertKey, ChannelID: channelID})
	if err != nil {
		return store.AlertDelivery{}, mapErr(err)
	}
	return alertFromRow(r), nil
}

func channelFromRow(r statedb.NotificationChannel) (store.NotificationChannel, error) {
	c := store.NotificationChannel{
		TenantID: r.TenantID, ID: r.ID, Name: r.Name, Kind: r.Kind, URLEnc: r.UrlEnc,
		WebhookMethod: r.WebhookMethod, WebhookTemplate: r.WebhookTemplate, IsDefault: r.IsDefault != 0,
		State: r.State, LastError: r.LastError, Revision: uint64(max(r.Revision, 0)),
		CreatedMs: r.CreatedMs, UpdatedMs: r.UpdatedMs,
	}
	if err := json.Unmarshal([]byte(r.WebhookHeadersJson), &c.WebhookHeaders); err != nil {
		return store.NotificationChannel{}, fmt.Errorf("channel %s: headers: %w", r.ID, err)
	}
	if err := json.Unmarshal([]byte(r.EventsJson), &c.Events); err != nil {
		return store.NotificationChannel{}, fmt.Errorf("channel %s: events: %w", r.ID, err)
	}
	return c, nil
}

func alertFromRow(r statedb.AlertOutbox) store.AlertDelivery {
	return store.AlertDelivery{
		TenantID: r.TenantID, AlertKey: r.AlertKey, ChannelID: r.ChannelID, MonitorID: r.MonitorID.String,
		Payload: json.RawMessage(r.PayloadJson), Attempts: int(r.Attempts), NextAttemptMs: r.NextAttemptMs,
		State: r.State, LastError: r.LastError, CreatedMs: r.CreatedMs, UpdatedMs: r.UpdatedMs,
	}
}
