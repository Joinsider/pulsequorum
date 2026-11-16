// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// Flapping (FA-ALR-06, ADR 0023) and the alert of a failed channel (FA-ALR-05)
// are FSM behavior of command level 4 and up. Below that level the FSM behaves
// exactly as before, so every voter computes the same state.
const (
	// FlappingCommandLevel is the command level that enables the rules of this
	// file and the events flapping and channel_failed (ADR 0023).
	FlappingCommandLevel = 4
	// flappingThreshold is the number of status changes within flappingWindow
	// (the new change included) that starts a flapping period.
	flappingThreshold = 6
	// flappingWindow is measured in command time (issued_at_ms).
	flappingWindow = 30 * time.Minute
)

// flappingEnabled reports whether the stored command level enables flapping
// and channel-failed alerts.
func flappingEnabled(ctx context.Context, tx store.StateTx) (bool, error) {
	stored, err := tx.CommandLevel(ctx)
	if err != nil {
		return false, fmt.Errorf("read command level: %w", err)
	}
	return EffectiveCommandLevel(stored) >= FlappingCommandLevel, nil
}

// flappingCountable reports whether a status takes part in flapping. Changes
// to PENDING, MAINTENANCE, UNKNOWN or PAUSED do not count.
func flappingCountable(s store.MonitorStatus) bool {
	switch s {
	case store.StatusUp, store.StatusDown, store.StatusDegraded, store.StatusPartial:
		return true
	}
	return false
}

// countFlappingChanges counts the changes that take part in flapping among
// changes.
func countFlappingChanges(changes []store.StatusChange) int {
	n := 0
	for _, c := range changes {
		if flappingCountable(c.Status) {
			n++
		}
	}
	return n
}

// flappingAlertKey identifies the alert that starts (end=false) or ends
// (end=true) the flapping period started at sinceMs (ADR 0023).
func flappingAlertKey(monitorID string, sinceMs int64, end bool) string {
	if end {
		return fmt.Sprintf("%s:%d:FLAPPING:END", monitorID, sinceMs)
	}
	return fmt.Sprintf("%s:%d:FLAPPING", monitorID, sinceMs)
}

// handleFlapping applies the flapping rules to a status change that has been
// written already, so the history contains it. It reports whether the normal
// alerts of the change are suppressed: because the monitor flaps, or because
// this change started the flapping period (ADR 0023, 4).
func handleFlapping(ctx context.Context, tx store.StateTx, cmd *pb.Command, set *pb.SetMonitorState, cur store.MonitorState, to store.MonitorStatus, since int64) (bool, error) {
	if cur.Flapping {
		return true, nil
	}
	if !flappingCountable(to) {
		return false, nil
	}
	enabled, err := flappingEnabled(ctx, tx)
	if err != nil || !enabled {
		return false, err
	}
	tenantID, monitorID, issuedAt := cmd.GetTenantId(), set.GetMonitorId(), cmd.GetIssuedAtMs()
	changes, err := tx.StatusChangesSince(ctx, tenantID, monitorID, issuedAt-flappingWindow.Milliseconds())
	if err != nil {
		return false, err
	}
	count := countFlappingChanges(changes)
	if count < flappingThreshold {
		return false, nil
	}
	if err := tx.SetMonitorFlapping(ctx, tenantID, monitorID, true, issuedAt); err != nil {
		return false, err
	}
	m, err := tx.GetMonitor(ctx, tenantID, monitorID)
	if err != nil {
		return false, err
	}
	p := AlertPayload{
		AlertID: flappingAlertKey(m.ID, issuedAt, false), TenantID: m.TenantID, MonitorID: m.ID,
		MonitorName: m.Name, MonitorType: m.Type, Event: store.EventFlapping,
		Status: string(to), PreviousStatus: string(cur.Status), SinceMs: since,
		Confirmed: set.GetConfirmed(), Flapping: true, ChangeCount: count,
	}
	if err := enqueueAlerts(ctx, tx, issuedAt, m, p); err != nil {
		return false, err
	}
	return true, nil
}

// applyClearMonitorFlapping ends a flapping period once the leader has seen it
// quiet (ADR 0023, 4). The period must still be the one the leader named.
func applyClearMonitorFlapping(ctx context.Context, tx store.StateTx, cmd *pb.Command, c *pb.ClearMonitorFlapping) (json.RawMessage, error) {
	tenantID, monitorID := cmd.GetTenantId(), c.GetMonitorId()
	if tenantID == "" || monitorID == "" {
		return nil, invalid("clear flapping without tenant or monitor")
	}
	enabled, err := flappingEnabled(ctx, tx)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, &CommandError{Code: CodeUnsupported, Message: fmt.Sprintf("clearing flapping needs command level %d", FlappingCommandLevel)}
	}
	st, err := tx.GetMonitorState(ctx, tenantID, monitorID)
	if err != nil {
		return nil, storeError(err)
	}
	if !st.Flapping || st.FlappingSinceMs != c.GetExpectedSinceMs() {
		return nil, &CommandError{Code: CodeConflict, Message: fmt.Sprintf("monitor %s is not flapping since %d", monitorID, c.GetExpectedSinceMs())}
	}
	changes, err := tx.StatusChangesSince(ctx, tenantID, monitorID, st.FlappingSinceMs)
	if err != nil {
		return nil, err
	}
	if err := tx.SetMonitorFlapping(ctx, tenantID, monitorID, false, 0); err != nil {
		return nil, storeError(err)
	}
	m, err := tx.GetMonitor(ctx, tenantID, monitorID)
	if err != nil {
		return nil, storeError(err)
	}
	p := AlertPayload{
		AlertID: flappingAlertKey(m.ID, st.FlappingSinceMs, true), TenantID: m.TenantID, MonitorID: m.ID,
		MonitorName: m.Name, MonitorType: m.Type, Event: store.EventFlapping,
		Status: string(st.Status), SinceMs: st.SinceMs, Confirmed: st.Confirmed,
		Flapping: false, ChangeCount: countFlappingChanges(changes),
	}
	if err := enqueueAlerts(ctx, tx, cmd.GetIssuedAtMs(), m, p); err != nil {
		return nil, err
	}
	return json.Marshal(PutResult{ID: m.ID})
}

// notifyChannelFailed tells the other default channels of the tenant that the
// channel failed (FA-ALR-05, ADR 0023, 5). The alert is attached to the monitor
// of the failed delivery, because the outbox refers to a monitor. A channel that
// is disabled gets nothing; the others get it once per state change.
func notifyChannelFailed(ctx context.Context, tx store.StateTx, cmd *pb.Command, failed store.AlertDelivery, ch store.NotificationChannel) error {
	tenantID, issuedAt := cmd.GetTenantId(), cmd.GetIssuedAtMs()
	channels, err := tx.ListChannels(ctx, tenantID)
	if err != nil {
		return err
	}
	var targets []store.NotificationChannel
	for _, o := range channels {
		if o.ID == ch.ID || !o.IsDefault || o.State == store.ChannelDisabled || !o.Subscribes(store.EventChannelFailed) {
			continue
		}
		targets = append(targets, o)
	}
	if len(targets) == 0 {
		return nil
	}
	m, err := tx.GetMonitor(ctx, tenantID, failed.MonitorID)
	if err != nil {
		return err
	}
	p := AlertPayload{
		AlertID: fmt.Sprintf("channel:%s:%s:%d", ch.ID, ch.State, issuedAt), TenantID: tenantID,
		MonitorID: m.ID, MonitorName: m.Name, MonitorType: m.Type, Event: store.EventChannelFailed,
		ChannelID: ch.ID, ChannelName: ch.Name, ChannelError: ch.LastError, Confirmed: true,
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return err
	}
	for _, o := range targets {
		err := tx.EnqueueAlert(ctx, store.AlertDelivery{
			TenantID: tenantID, AlertKey: p.AlertID, ChannelID: o.ID, MonitorID: m.ID, Payload: payload,
			NextAttemptMs: issuedAt, State: store.AlertPending, CreatedMs: issuedAt, UpdatedMs: issuedAt,
		})
		if err != nil {
			return err
		}
	}
	return nil
}
