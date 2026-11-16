// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/secrets"
	"github.com/joinsider/uptimecluster/internal/store"
)

// alertRetention is how long finished outbox entries are kept, in command time.
const alertRetention = 7 * 24 * time.Hour

// alertEvent returns the notification event of a status change, or "" if the
// change is not notified (ADR 0014, FA-ALR-02): PENDING, PAUSED and the first
// UP of a new monitor stay silent.
func alertEvent(from, to store.MonitorStatus) string {
	switch to {
	case store.StatusDown:
		return store.EventDown
	case store.StatusUp:
		if from == store.StatusDown {
			return store.EventUp
		}
	case store.StatusDegraded:
		if from == store.StatusDown {
			return store.EventUp
		}
		return store.EventDegraded
	case store.StatusPartial:
		return store.EventPartial
	}
	return ""
}

// enqueueAlerts adds one outbox entry per subscribed, enabled channel of the
// monitor, in the same transaction as the status change.
func enqueueAlerts(ctx context.Context, tx store.StateTx, issuedAt int64, m store.Monitor, p AlertPayload) error {
	payload, err := json.Marshal(p)
	if err != nil {
		return err
	}
	for _, id := range m.ChannelIDs {
		ch, err := tx.GetChannel(ctx, m.TenantID, id)
		if err != nil {
			return err
		}
		if ch.State == store.ChannelDisabled || !ch.Subscribes(p.Event) {
			continue
		}
		err = tx.EnqueueAlert(ctx, store.AlertDelivery{
			TenantID: m.TenantID, AlertKey: p.AlertID, ChannelID: id, MonitorID: m.ID, Payload: payload,
			NextAttemptMs: issuedAt, State: store.AlertPending, CreatedMs: issuedAt, UpdatedMs: issuedAt,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func alertKey(monitorID string, sinceMs int64, status store.MonitorStatus) string {
	return fmt.Sprintf("%s:%d:%s", monitorID, sinceMs, status)
}

// notifyStateChange enqueues the alerts of a status change.
func notifyStateChange(ctx context.Context, tx store.StateTx, cmd *pb.Command, set *pb.SetMonitorState, from, to store.MonitorStatus, since int64) error {
	event := alertEvent(from, to)
	if event == "" {
		return nil
	}
	m, err := tx.GetMonitor(ctx, cmd.GetTenantId(), set.GetMonitorId())
	if err != nil {
		return err
	}
	p := AlertPayload{
		AlertID: alertKey(m.ID, since, to), TenantID: m.TenantID, MonitorID: m.ID, MonitorName: m.Name,
		MonitorType: m.Type, Event: event, Status: string(to), PreviousStatus: string(from), SinceMs: since,
		Confirmed: set.GetConfirmed(),
	}
	if inc := set.GetIncident(); to == store.StatusDown && inc != nil {
		p.Cause, p.ErrorClass, p.IncidentID = inc.GetCause(), ErrorClassFromProto(inc.GetErrorClass()), inc.GetIncidentId()
	}
	return enqueueAlerts(ctx, tx, cmd.GetIssuedAtMs(), m, p)
}

// applyEnqueueAlert repeats the DOWN alert while the monitor stays in the
// same DOWN period (FA-ALR-02).
func applyEnqueueAlert(ctx context.Context, tx store.StateTx, cmd *pb.Command, e *pb.EnqueueAlert) (json.RawMessage, error) {
	if cmd.GetTenantId() == "" || e.GetMonitorId() == "" || e.GetReminder() == 0 {
		return nil, invalid("reminder without tenant, monitor or number")
	}
	m, err := tx.GetMonitor(ctx, cmd.GetTenantId(), e.GetMonitorId())
	if err != nil {
		return nil, storeError(err)
	}
	if m.State.Status != store.StatusDown || m.State.SinceMs != e.GetDownSinceMs() {
		return nil, &CommandError{Code: CodeConflict, Message: fmt.Sprintf("monitor %s is no longer in that DOWN period", m.ID)}
	}
	if m.State.Flapping {
		// A flapping monitor sends its summary only; reminders wait (ADR 0023, 4).
		return json.Marshal(PutResult{})
	}
	p := AlertPayload{
		AlertID:  fmt.Sprintf("%s:reminder:%d", alertKey(m.ID, m.State.SinceMs, store.StatusDown), e.GetReminder()),
		TenantID: m.TenantID, MonitorID: m.ID, MonitorName: m.Name, MonitorType: m.Type, Event: store.EventDown,
		Status: string(store.StatusDown), PreviousStatus: string(store.StatusDown), SinceMs: m.State.SinceMs,
		Reminder: int(e.GetReminder()), Confirmed: m.State.Confirmed,
	}
	if err := enqueueAlerts(ctx, tx, cmd.GetIssuedAtMs(), m, p); err != nil {
		return nil, storeError(err)
	}
	return json.Marshal(PutResult{ID: p.AlertID})
}

// applyAckAlertDelivery records a delivery attempt. Acks for another attempt
// or a finished entry are rejected, so duplicates change nothing.
func applyAckAlertDelivery(ctx context.Context, tx store.StateTx, cmd *pb.Command, ack *pb.AckAlertDelivery) (json.RawMessage, error) {
	a, err := tx.GetAlert(ctx, ack.GetAlertKey(), ack.GetChannelId())
	if err != nil {
		return nil, storeError(err)
	}
	if a.TenantID != cmd.GetTenantId() {
		return nil, &CommandError{Code: CodeNotFound, Message: "alert not found"}
	}
	if a.State != store.AlertPending || uint32(a.Attempts)+1 != ack.GetAttempt() { //nolint:gosec // attempts are small
		return nil, &CommandError{Code: CodeConflict, Message: fmt.Sprintf("alert %s is %s after %d attempts", a.AlertKey, a.State, a.Attempts)}
	}
	now := cmd.GetIssuedAtMs()
	a.Attempts = int(ack.GetAttempt())
	a.LastError = ack.GetError()
	a.UpdatedMs = now
	channelState, channelErr := "", ""
	switch {
	case ack.GetDelivered():
		a.State, a.LastError = store.AlertDelivered, ""
		channelState = store.ChannelOK
	case ack.GetPermanentFailure():
		a.State = store.AlertFailed
		channelState, channelErr = store.ChannelDisabled, ack.GetError()
	case ack.GetGiveUp():
		a.State = store.AlertFailed
		channelState, channelErr = store.ChannelFailing, ack.GetError()
	default:
		a.NextAttemptMs = max(ack.GetNextAttemptMs(), now)
	}
	if err := tx.UpdateAlert(ctx, a); err != nil {
		return nil, err
	}
	if channelState != "" {
		prev, err := tx.GetChannel(ctx, a.TenantID, a.ChannelID)
		if err != nil {
			return nil, storeError(err)
		}
		if err := tx.SetChannelState(ctx, a.TenantID, a.ChannelID, channelState, channelErr); err != nil {
			return nil, err
		}
		enabled, err := flappingEnabled(ctx, tx)
		if err != nil {
			return nil, err
		}
		// Only a channel that fails notifies; recovery does not (ADR 0023, 5).
		if enabled && channelState != store.ChannelOK && prev.State != channelState {
			failed := prev
			failed.State, failed.LastError = channelState, channelErr
			if err := notifyChannelFailed(ctx, tx, cmd, a, failed); err != nil {
				return nil, storeError(err)
			}
		}
	}
	if err := tx.PruneAlerts(ctx, now-alertRetention.Milliseconds()); err != nil {
		return nil, err
	}
	return json.Marshal(PutResult{ID: a.AlertKey})
}

// channelFromDocument validates a channel document; secrets must be encrypted.
//
// extended admits the events of command level 4 (ADR 0023); below that level
// they are unknown, as before.
func channelFromDocument(tenantID, id string, doc ChannelDocument, issuedAt int64, extended bool) (store.NotificationChannel, error) {
	switch {
	case tenantID == "":
		return store.NotificationChannel{}, invalid("channel command without tenant")
	case doc.Kind != store.ChannelShoutrrr && doc.Kind != store.ChannelWebhook:
		return store.NotificationChannel{}, invalid("channel kind %q", doc.Kind)
	case !secrets.IsEncrypted(doc.URLEnc):
		return store.NotificationChannel{}, invalid("channel URL must be encrypted")
	}
	for name, v := range doc.WebhookHeaders {
		if !secrets.IsEncrypted(v) {
			return store.NotificationChannel{}, invalid("header %q must be encrypted", name)
		}
	}
	for _, e := range doc.Events {
		switch e {
		case store.EventDown, store.EventUp, store.EventDegraded, store.EventPartial:
		case store.EventFlapping, store.EventChannelFailed:
			if !extended {
				return store.NotificationChannel{}, invalid("event %q", e)
			}
		default:
			return store.NotificationChannel{}, invalid("event %q", e)
		}
	}
	return store.NotificationChannel{
		TenantID: tenantID, ID: id, Name: doc.Name, Kind: doc.Kind, URLEnc: doc.URLEnc,
		WebhookMethod: doc.WebhookMethod, WebhookHeaders: doc.WebhookHeaders, WebhookTemplate: doc.WebhookTemplate,
		Events: doc.Events, IsDefault: doc.IsDefault, CreatedMs: issuedAt, UpdatedMs: issuedAt,
	}, nil
}
