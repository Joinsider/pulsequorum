// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

// The degraded mode (FA-ALR-10, FA-ALR-11, FA-UPT-07, ADR 0024) is FSM behavior
// of command level 5 and up. Below that level the commands are rejected and no
// command time is stored, so mixed-version clusters keep identical fingerprints.
const (
	// DegradedCommandLevel is the command level that enables the degraded-mode
	// commands and the stored command time (ADR 0024).
	DegradedCommandLevel = 5
	// maxDegradedBatch bounds the entries of one report or verdict command.
	maxDegradedBatch = 500
)

// DegradedReportResult is the result of ReportDegradedAlerts.
type DegradedReportResult struct {
	Count int `json:"count"`
}

// UnconfirmedResolveResult is the result of ResolveUnconfirmedAlerts: how many
// verdicts were stored. Already judged alerts are skipped and not counted.
type UnconfirmedResolveResult struct {
	Resolved int `json:"resolved"`
}

// degradedEnabled reports whether the stored command level enables the
// degraded-mode commands and the stored command time.
func degradedEnabled(ctx context.Context, tx store.StateTx) (bool, error) {
	stored, err := tx.CommandLevel(ctx)
	if err != nil {
		return false, fmt.Errorf("read command level: %w", err)
	}
	return EffectiveCommandLevel(stored) >= DegradedCommandLevel, nil
}

// requireDegradedLevel rejects a degraded-mode command below its command level.
func requireDegradedLevel(ctx context.Context, tx store.StateTx, what string) error {
	enabled, err := degradedEnabled(ctx, tx)
	if err != nil {
		return err
	}
	if !enabled {
		return &CommandError{Code: CodeUnsupported, Message: fmt.Sprintf("%s needs command level %d", what, DegradedCommandLevel)}
	}
	return nil
}

// recordIssuedAt stores the command time of an applied command as the time of
// the last applied command (ADR 0024). Below level 5 nothing is written.
func recordIssuedAt(ctx context.Context, tx store.StateTx, cmd *pb.Command) error {
	enabled, err := degradedEnabled(ctx, tx)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	if err := tx.SetLastIssuedAt(ctx, cmd.GetIssuedAtMs()); err != nil {
		return fmt.Errorf("store last issued time: %w", err)
	}
	return nil
}

// validVerdict reports whether verdict is one of the store.Verdict constants.
func validVerdict(verdict string) bool {
	switch verdict {
	case store.VerdictConfirmed, store.VerdictFalseAlarm, store.VerdictUnverifiable:
		return true
	}
	return false
}

// checkDegradedEntry validates one alert of a report. Only DOWN and UP are
// announced by a degraded node (proto DegradedAlertEntry.status).
func checkDegradedEntry(e *pb.DegradedAlertEntry) error {
	switch {
	case e.GetLocalId() <= 0:
		return invalid("degraded alert without local ID")
	case e.GetTenantId() == "" || e.GetMonitorId() == "" || e.GetChannelId() == "":
		return invalid("degraded alert %d without tenant, monitor or channel", e.GetLocalId())
	}
	switch store.MonitorStatus(e.GetStatus()) {
	case store.StatusDown, store.StatusUp:
		return nil
	}
	return invalid("degraded alert %d has status %q", e.GetLocalId(), e.GetStatus())
}

// applyReportDegradedAlerts stores the alerts of a node that were sent while it
// was degraded (FA-ALR-10, ADR 0024). Entries are stored idempotently by
// (node, local ID), so a repeated block changes nothing.
func applyReportDegradedAlerts(ctx context.Context, tx store.StateTx, cmd *pb.Command, r *pb.ReportDegradedAlerts) (json.RawMessage, error) {
	if err := requireDegradedLevel(ctx, tx, "reporting degraded alerts"); err != nil {
		return nil, err
	}
	if r.GetNodeId() == "" {
		return nil, invalid("degraded report without node")
	}
	n := len(r.GetEntries())
	if n < 1 || n > maxDegradedBatch {
		return nil, invalid("degraded report with %d entries, want 1..%d", n, maxDegradedBatch)
	}
	alerts := make([]store.UnconfirmedAlert, 0, n)
	for _, e := range r.GetEntries() {
		if err := checkDegradedEntry(e); err != nil {
			return nil, err
		}
		alerts = append(alerts, store.UnconfirmedAlert{
			NodeID:     r.GetNodeId(),
			LocalID:    e.GetLocalId(),
			TenantID:   e.GetTenantId(),
			MonitorID:  e.GetMonitorId(),
			ChannelID:  e.GetChannelId(),
			Status:     e.GetStatus(),
			SinceMs:    e.GetSinceMs(),
			SentMs:     e.GetSentMs(),
			Outcome:    e.GetOutcome(),
			Reachable:  int(e.GetReachable()),
			Total:      int(e.GetTotal()),
			Reporters:  e.GetReporters(),
			ReportedMs: cmd.GetIssuedAtMs(),
		})
	}
	if err := tx.InsertUnconfirmedAlerts(ctx, alerts); err != nil {
		return nil, storeError(err)
	}
	return json.Marshal(DegradedReportResult{Count: n})
}

// applyResolveUnconfirmedAlerts stores the verdicts of the leader (FA-ALR-10,
// ADR 0024). An alert that already has a verdict is skipped, so a repeated
// command changes nothing. A verdict other than unverifiable notifies the
// channel of the alert, if the leader asked for it and the channel takes the event.
func applyResolveUnconfirmedAlerts(ctx context.Context, tx store.StateTx, cmd *pb.Command, r *pb.ResolveUnconfirmedAlerts) (json.RawMessage, error) {
	if err := requireDegradedLevel(ctx, tx, "resolving unconfirmed alerts"); err != nil {
		return nil, err
	}
	n := len(r.GetVerdicts())
	if n < 1 || n > maxDegradedBatch {
		return nil, invalid("verdicts %d, want 1..%d", n, maxDegradedBatch)
	}
	for _, v := range r.GetVerdicts() {
		switch {
		case v.GetNodeId() == "" || v.GetLocalId() <= 0:
			return nil, invalid("verdict without node or local ID")
		case !validVerdict(v.GetVerdict()):
			return nil, invalid("verdict %q for %s/%d", v.GetVerdict(), v.GetNodeId(), v.GetLocalId())
		}
	}
	issuedAt := cmd.GetIssuedAtMs()
	resolved := 0
	for _, v := range r.GetVerdicts() {
		ua, err := tx.GetUnconfirmedAlert(ctx, v.GetNodeId(), v.GetLocalId())
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, invalid("unconfirmed alert %s/%d not found", v.GetNodeId(), v.GetLocalId())
			}
			return nil, err
		}
		err = tx.ResolveUnconfirmedAlert(ctx, v.GetNodeId(), v.GetLocalId(), v.GetVerdict(), issuedAt)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		if err != nil {
			return nil, storeError(err)
		}
		resolved++
		if v.GetNotify() && v.GetVerdict() != store.VerdictUnverifiable {
			if err := enqueueVerdictAlert(ctx, tx, issuedAt, ua, v.GetVerdict()); err != nil {
				return nil, storeError(err)
			}
		}
	}
	return json.Marshal(UnconfirmedResolveResult{Resolved: resolved})
}

// verdictEvent maps the status announced by an unconfirmed alert to its event.
// Only DOWN and UP are announced by a degraded node (see checkDegradedEntry).
func verdictEvent(status string) string {
	switch store.MonitorStatus(status) {
	case store.StatusDown:
		return store.EventDown
	case store.StatusUp:
		return store.EventUp
	}
	return ""
}

// enqueueVerdictAlert sends the correction of an unconfirmed alert to its
// channel, and only to that channel (ADR 0024). A missing or disabled channel,
// or one that does not take the event, gets nothing. So does an alert of a
// monitor that was deleted since: the outbox refers to its monitor (foreign key),
// and a verdict must not reject the whole batch.
func enqueueVerdictAlert(ctx context.Context, tx store.StateTx, issuedAt int64, ua store.UnconfirmedAlert, verdict string) error {
	event := verdictEvent(ua.Status)
	if event == "" {
		return nil
	}
	ch, err := tx.GetChannel(ctx, ua.TenantID, ua.ChannelID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if ch.State == store.ChannelDisabled || !ch.Subscribes(event) {
		return nil
	}
	m, err := tx.GetMonitor(ctx, ua.TenantID, ua.MonitorID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	key := fmt.Sprintf("verdict:%s:%d", ua.NodeID, ua.LocalID)
	payload, err := json.Marshal(AlertPayload{
		AlertID: key, TenantID: ua.TenantID, MonitorID: ua.MonitorID, MonitorName: m.Name,
		MonitorType: m.Type, Event: event, Status: ua.Status, SinceMs: ua.SinceMs,
		Verdict: verdict, Confirmed: verdict == store.VerdictConfirmed,
		Sender: &AlertSender{
			NodeID: ua.NodeID, ReachableVoters: ua.Reachable, TotalVoters: ua.Total,
			Reporters: ua.Reporters, Confirmed: false,
		},
	})
	if err != nil {
		return err
	}
	return enqueueAlertDelivery(ctx, tx, issuedAt, store.AlertDelivery{
		TenantID: ua.TenantID, AlertKey: key, ChannelID: ch.ID, MonitorID: ua.MonitorID, Payload: payload,
	})
}

// applyRecordMonitorHistory replaces the history of a monitor in a gap that was
// re-evaluated after the quorum returned (FA-UPT-07, ADR 0024). Incidents of the
// gap are recorded as reevaluated and no notification is sent. The state becomes
// the last change of the gap.
func applyRecordMonitorHistory(ctx context.Context, tx store.StateTx, cmd *pb.Command, r *pb.RecordMonitorHistory) (json.RawMessage, error) {
	if err := requireDegradedLevel(ctx, tx, "recording monitor history"); err != nil {
		return nil, err
	}
	if err := validateHistoryCommand(cmd.GetTenantId(), r); err != nil {
		return nil, err
	}
	tenantID, monitorID := cmd.GetTenantId(), r.GetMonitorId()
	from, to := r.GetFromMs(), r.GetToMs()
	entries := r.GetEntries()
	expected := store.MonitorStatus(r.GetExpectedStatus())

	cur, err := tx.GetMonitorState(ctx, tenantID, monitorID)
	if err != nil {
		return nil, storeError(err)
	}
	if err := checkCurrentStateForHistory(monitorID, cur, expected, from); err != nil {
		return nil, err
	}

	changes := make([]store.StatusChange, 0, len(entries))
	for _, e := range entries {
		changes = append(changes, store.StatusChange{SinceMs: e.GetSinceMs(), Status: store.MonitorStatus(e.GetStatus())})
	}
	if err := tx.ReplaceStatusHistory(ctx, tenantID, monitorID, from, to, changes); err != nil {
		return nil, storeError(err)
	}
	// A monitor that was DOWN before the gap ends its open incident at the first change.
	if cur.Status == store.StatusDown {
		if err := tx.ResolveIncident(ctx, tenantID, monitorID, entries[0].GetSinceMs()); err != nil {
			return nil, storeError(err)
		}
	}
	if err := openReevaluatedIncidents(ctx, tx, tenantID, monitorID, entries); err != nil {
		return nil, err
	}

	last := entries[len(entries)-1]
	lastStatus := store.MonitorStatus(last.GetStatus())
	if err := tx.PutMonitorState(ctx, tenantID, monitorID, store.MonitorState{
		Status: lastStatus, Confirmed: true, SinceMs: last.GetSinceMs(),
	}); err != nil {
		return nil, storeError(err)
	}
	ev := StateEvent{TenantID: tenantID, MonitorID: monitorID, Status: lastStatus, Confirmed: true,
		SinceMs: last.GetSinceMs(), IncidentResolved: cur.Status == store.StatusDown}
	if lastStatus == store.StatusDown {
		ev.IncidentOpened = last.GetIncidentId()
	}
	recordStateEvent(ctx, ev)
	if err := tx.PruneStatusHistory(ctx, tenantID, monitorID, cmd.GetIssuedAtMs()-statusHistoryRetention.Milliseconds()); err != nil {
		return nil, err
	}
	return json.Marshal(StateResult{Status: lastStatus})
}

func validateHistoryCommand(tenantID string, r *pb.RecordMonitorHistory) error {
	monitorID := r.GetMonitorId()
	from, to := r.GetFromMs(), r.GetToMs()
	entries := r.GetEntries()
	expected := store.MonitorStatus(r.GetExpectedStatus())
	switch {
	case tenantID == "" || monitorID == "":
		return invalid("monitor history without tenant or monitor")
	case from >= to:
		return invalid("history interval [%d, %d) is empty", from, to)
	case len(entries) == 0:
		return invalid("monitor history without entries")
	case !expected.Valid() || expected == store.StatusPaused || expected == store.StatusMaintenance:
		return invalid("expected status %q", expected)
	}
	prev := expected
	for i, e := range entries {
		var prevMs int64
		if i > 0 {
			prevMs = entries[i-1].GetSinceMs()
		}
		st, err := validateHistoryEntry(e, i > 0, prevMs, from, to, prev)
		if err != nil {
			return err
		}
		prev = st
	}
	return nil
}

func validateHistoryEntry(e *pb.HistoryEntry, hasPrev bool, prevMs, from, to int64, prev store.MonitorStatus) (store.MonitorStatus, error) {
	st := store.MonitorStatus(e.GetStatus())
	switch {
	case e.GetSinceMs() < from || e.GetSinceMs() >= to:
		return "", invalid("history entry at %d outside [%d, %d)", e.GetSinceMs(), from, to)
	case hasPrev && e.GetSinceMs() <= prevMs:
		return "", invalid("history entries not strictly ascending at %d", e.GetSinceMs())
	case !st.Valid() || st == store.StatusPaused || st == store.StatusMaintenance:
		return "", invalid("history status %q cannot be recorded", st)
	case st == prev:
		return "", invalid("history entry at %d repeats status %s", e.GetSinceMs(), st)
	case st == store.StatusDown && e.GetIncidentId() == "":
		return "", invalid("DOWN history entry at %d without incident", e.GetSinceMs())
	}
	return st, nil
}

func checkCurrentStateForHistory(monitorID string, cur store.MonitorState, expected store.MonitorStatus, from int64) error {
	switch {
	case cur.Status == store.StatusPaused || cur.Status == store.StatusMaintenance:
		return &CommandError{Code: CodeConflict, Message: fmt.Sprintf("monitor %s is %s", monitorID, cur.Status)}
	case cur.Status != expected:
		return &CommandError{Code: CodeConflict, Message: fmt.Sprintf("monitor %s is %s, not %s", monitorID, cur.Status, expected)}
	case from < cur.SinceMs:
		return &CommandError{Code: CodeConflict, Message: fmt.Sprintf("history starts at %d, before the current state since %d", from, cur.SinceMs)}
	}
	return nil
}

func openReevaluatedIncidents(ctx context.Context, tx store.StateTx, tenantID, monitorID string, entries []*pb.HistoryEntry) error {
	for i, e := range entries {
		if store.MonitorStatus(e.GetStatus()) != store.StatusDown {
			continue
		}
		// A DOWN period ends at the next change; the last one stays open.
		var resolvedMs int64
		if i+1 < len(entries) {
			resolvedMs = entries[i+1].GetSinceMs()
		}
		err := tx.OpenIncident(ctx, store.Incident{
			TenantID:    tenantID,
			ID:          e.GetIncidentId(),
			MonitorID:   monitorID,
			StartedMs:   e.GetSinceMs(),
			ResolvedMs:  resolvedMs,
			Cause:       e.GetCause(),
			ErrorClass:  e.GetErrorClass(),
			Confirmed:   true,
			Reevaluated: true,
		})
		if err != nil {
			return storeError(err)
		}
	}
	return nil
}
