// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// reportAlert is an alert of node n2 as the node stores it in its local log.
func reportAlert(localID int64, monitorID, channelID string, status store.MonitorStatus, sinceMs int64) store.DegradedAlert {
	return store.DegradedAlert{
		ID: localID, TenantID: "t1", MonitorID: monitorID, ChannelID: channelID, Status: string(status),
		SinceMs: sinceMs, SentMs: sinceMs + 50, Outcome: "sent", Reachable: 1, Total: 3, Reporters: []string{"n2"},
	}
}

// verdictPayload decodes the payload of an outbox entry.
func verdictPayload(t *testing.T, a store.AlertDelivery) AlertPayload {
	t.Helper()
	var p AlertPayload
	if err := json.Unmarshal(a.Payload, &p); err != nil {
		t.Fatalf("payload of %s/%s: %v", a.AlertKey, a.ChannelID, err)
	}
	return p
}

// TestDegradedCommandsNeedLevel5 covers ADR 0024: the three commands are
// rejected below command level 5 and do not touch state.
func TestDegradedCommandsNeedLevel5(t *testing.T) {
	tests := []struct {
		name string
		cmd  *pb.Command
	}{
		{"report degraded alerts", NewReportDegradedAlerts("n2", []store.DegradedAlert{reportAlert(1, "m1", "a", store.StatusDown, 1500)})},
		{"resolve unconfirmed alerts", NewResolveUnconfirmedAlerts([]AlertVerdict{{NodeID: "n2", LocalID: 1, Verdict: store.VerdictConfirmed, Notify: true}})},
		{"record monitor history", NewRecordMonitorHistory("t1", "m1", store.StatusUp, 20000, 60000,
			[]HistoryChange{{SinceMs: 30000, Status: store.StatusDown, IncidentID: "g1"}})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := flapSetup(t, 4)
			h.changeState(10100, "t1", "m1", store.StatusPending, store.StatusUp, 10000, "")
			h.do(tt.cmd, 3000, ErrInvalidCommand)
			if st := h.monitorState("m1"); st.Status != store.StatusUp || st.SinceMs != 10000 {
				t.Errorf("monitor changed by rejected command: %+v", st)
			}
		})
	}
}

// TestReportDegradedAlertsValidation covers the checks of a report (FA-ALR-10).
func TestReportDegradedAlertsValidation(t *testing.T) {
	many := make([]store.DegradedAlert, 501)
	for i := range many {
		many[i] = reportAlert(int64(i+1), "m1", "a", store.StatusDown, 1500)
	}
	tests := []struct {
		name   string
		nodeID string
		alerts []store.DegradedAlert
		want   error
	}{
		{"valid DOWN", "n2", []store.DegradedAlert{reportAlert(1, "m1", "a", store.StatusDown, 1500)}, nil},
		{"valid UP", "n2", []store.DegradedAlert{reportAlert(1, "m1", "a", store.StatusUp, 1500)}, nil},
		{"batch of 500", "n2", many[:500], nil},
		{"no node", "", []store.DegradedAlert{reportAlert(1, "m1", "a", store.StatusDown, 1500)}, ErrInvalidCommand},
		{"no entries", "n2", nil, ErrInvalidCommand},
		{"501 entries", "n2", many, ErrInvalidCommand},
		{"local ID zero", "n2", []store.DegradedAlert{reportAlert(0, "m1", "a", store.StatusDown, 1500)}, ErrInvalidCommand},
		{"missing tenant", "n2", []store.DegradedAlert{func() store.DegradedAlert {
			a := reportAlert(1, "m1", "a", store.StatusDown, 1500)
			a.TenantID = ""
			return a
		}()}, ErrInvalidCommand},
		{"missing channel", "n2", []store.DegradedAlert{reportAlert(1, "m1", "", store.StatusDown, 1500)}, ErrInvalidCommand},
		{"status PAUSED", "n2", []store.DegradedAlert{reportAlert(1, "m1", "a", store.StatusPaused, 1500)}, ErrInvalidCommand},
		{"status PENDING", "n2", []store.DegradedAlert{reportAlert(1, "m1", "a", store.StatusPending, 1500)}, ErrInvalidCommand},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newAlertFSM(t, t.TempDir())
			h.setLevel(5)
			h.do(NewReportDegradedAlerts(tt.nodeID, tt.alerts), 2000, tt.want)
		})
	}
}

// TestReportDegradedAlertsIdempotent: a repeated command is a duplicate, and a
// repeated entry under a new request ID leaves the stored alert unchanged.
func TestReportDegradedAlertsIdempotent(t *testing.T) {
	h := newAlertFSM(t, t.TempDir())
	h.setLevel(5)
	first := NewReportDegradedAlerts("n2", []store.DegradedAlert{reportAlert(1, "m1", "a", store.StatusDown, 1500)})
	if res := h.apply(first, 2000); res.Err != nil {
		t.Fatal(res.Err)
	}
	if res := h.apply(first, 2100); !res.Duplicate {
		t.Fatalf("same command again: Duplicate = false")
	}

	changed := reportAlert(1, "m1", "a", store.StatusDown, 1500)
	changed.Reachable = 2
	again := NewReportDegradedAlerts("n2", []store.DegradedAlert{changed})
	again.RequestId = "retry-with-new-id"
	if res := h.apply(again, 2200); res.Err != nil {
		t.Fatal(res.Err)
	}
	got, err := h.unconfirmed("n2", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reachable != 1 || got.ReportedMs != 2000 || got.Verdict != "" {
		t.Errorf("stored alert changed by repeated entry: %+v", got)
	}
}

// TestResolveUnconfirmedAlerts covers the verdicts (FA-ALR-10, ADR 0024): one
// outbox entry for one channel, none for unverifiable, notify=false, disabled
// or unsubscribed channels, and a second verdict is skipped.
func TestResolveUnconfirmedAlerts(t *testing.T) {
	h := newAlertFSM(t, t.TempDir())
	h.setLevel(5)
	h.putTenant(1000, "t1")
	h.putChannel(1000, "t1", "a", channel(allEvents...), nil)
	h.putChannel(1000, "t1", "b", channel(store.EventDown), nil)
	h.putChannel(1000, "t1", "c", channel(store.EventUp), nil)
	h.putMonitor(1000, "t1", "m1", "a")
	h.do(NewReportDegradedAlerts("n2", []store.DegradedAlert{
		reportAlert(1, "m1", "a", store.StatusDown, 1500),
		reportAlert(2, "m1", "a", store.StatusUp, 1600),
		reportAlert(3, "m1", "a", store.StatusDown, 1700),
		reportAlert(4, "m1", "a", store.StatusDown, 1800),
		reportAlert(5, "m1", "b", store.StatusDown, 1900),
		reportAlert(6, "m1", "c", store.StatusDown, 1950),
		reportAlert(7, "gone", "a", store.StatusUp, 2000),
	}), 2000, nil)

	t.Run("confirmed DOWN notifies its channel once", func(t *testing.T) {
		h.t = t
		h.do(NewResolveUnconfirmedAlerts([]AlertVerdict{{NodeID: "n2", LocalID: 1, Verdict: store.VerdictConfirmed, Notify: true}}), 3000, nil)
		a := h.alert("verdict:n2:1", "a")
		p := verdictPayload(t, a)
		want := AlertPayload{
			AlertID: "verdict:n2:1", TenantID: "t1", MonitorID: "m1", MonitorName: "Web m1", MonitorType: "http",
			Event: store.EventDown, Status: "DOWN", SinceMs: 1500, Verdict: store.VerdictConfirmed, Confirmed: true,
			Sender: &AlertSender{NodeID: "n2", ReachableVoters: 1, TotalVoters: 3, Reporters: []string{"n2"}},
		}
		if p.AlertID != want.AlertID || p.Event != want.Event || p.Status != want.Status || p.SinceMs != want.SinceMs ||
			p.Verdict != want.Verdict || !p.Confirmed || p.MonitorName != want.MonitorName || p.Sender == nil ||
			p.Sender.NodeID != "n2" || p.Sender.ReachableVoters != 1 || p.Sender.TotalVoters != 3 ||
			p.Sender.Confirmed || len(p.Sender.Reporters) != 1 {
			t.Errorf("payload = %+v, want like %+v", p, want)
		}
		h.noAlert("verdict:n2:1", "b")
		h.noAlert("verdict:n2:1", "c")
	})

	t.Run("false alarm UP notifies with event up", func(t *testing.T) {
		h.t = t
		h.do(NewResolveUnconfirmedAlerts([]AlertVerdict{{NodeID: "n2", LocalID: 2, Verdict: store.VerdictFalseAlarm, Notify: true}}), 3100, nil)
		p := verdictPayload(t, h.alert("verdict:n2:2", "a"))
		if p.Event != store.EventUp || p.Verdict != store.VerdictFalseAlarm || p.Confirmed {
			t.Errorf("payload = %+v, want event up, false_alarm, not confirmed", p)
		}
	})

	t.Run("unverifiable is never notified", func(t *testing.T) {
		h.t = t
		h.do(NewResolveUnconfirmedAlerts([]AlertVerdict{{NodeID: "n2", LocalID: 3, Verdict: store.VerdictUnverifiable, Notify: true}}), 3200, nil)
		h.noAlert("verdict:n2:3", "a")
	})

	t.Run("notify false is silent", func(t *testing.T) {
		h.t = t
		h.do(NewResolveUnconfirmedAlerts([]AlertVerdict{{NodeID: "n2", LocalID: 4, Verdict: store.VerdictConfirmed, Notify: false}}), 3300, nil)
		h.noAlert("verdict:n2:4", "a")
	})

	t.Run("disabled channel is silent", func(t *testing.T) {
		h.t = t
		if err := h.st.Update(h.ctx, func(tx store.StateTx) error {
			return tx.SetChannelState(h.ctx, "t1", "b", store.ChannelDisabled, "gone")
		}); err != nil {
			t.Fatal(err)
		}
		h.do(NewResolveUnconfirmedAlerts([]AlertVerdict{{NodeID: "n2", LocalID: 5, Verdict: store.VerdictConfirmed, Notify: true}}), 3400, nil)
		h.noAlert("verdict:n2:5", "b")
	})

	t.Run("channel not subscribed to the event is silent", func(t *testing.T) {
		h.t = t
		h.do(NewResolveUnconfirmedAlerts([]AlertVerdict{{NodeID: "n2", LocalID: 6, Verdict: store.VerdictConfirmed, Notify: true}}), 3500, nil)
		h.noAlert("verdict:n2:6", "c")
	})

	t.Run("deleted monitor is judged but not notified", func(t *testing.T) {
		h.t = t
		h.do(NewResolveUnconfirmedAlerts([]AlertVerdict{{NodeID: "n2", LocalID: 7, Verdict: store.VerdictFalseAlarm, Notify: true}}), 3600, nil)
		h.noAlert("verdict:n2:7", "a")
		if got, err := h.unconfirmed("n2", 7); err != nil || got.Verdict != store.VerdictFalseAlarm {
			t.Errorf("alert 7 = %+v, %v; want verdict false_alarm", got, err)
		}
	})

	t.Run("second verdict is skipped", func(t *testing.T) {
		h.t = t
		cmd := NewResolveUnconfirmedAlerts([]AlertVerdict{{NodeID: "n2", LocalID: 1, Verdict: store.VerdictFalseAlarm, Notify: true}})
		res := h.apply(cmd, 3700)
		if res.Err != nil {
			t.Fatal(res.Err)
		}
		var out UnconfirmedResolveResult
		if err := json.Unmarshal(res.Result, &out); err != nil {
			t.Fatal(err)
		}
		if out.Resolved != 0 {
			t.Errorf("resolved = %d, want 0", out.Resolved)
		}
		got, err := h.unconfirmed("n2", 1)
		if err != nil {
			t.Fatal(err)
		}
		if got.Verdict != store.VerdictConfirmed {
			t.Errorf("verdict changed to %q", got.Verdict)
		}
		if p := verdictPayload(t, h.alert("verdict:n2:1", "a")); p.Verdict != store.VerdictConfirmed {
			t.Errorf("outbox entry changed to verdict %q", p.Verdict)
		}
	})

	validation := []struct {
		name     string
		verdicts []AlertVerdict
		want     error
	}{
		{"unknown alert", []AlertVerdict{{NodeID: "n9", LocalID: 1, Verdict: store.VerdictConfirmed}}, ErrInvalidCommand},
		{"unknown verdict", []AlertVerdict{{NodeID: "n2", LocalID: 1, Verdict: "maybe"}}, ErrInvalidCommand},
		{"no verdicts", nil, ErrInvalidCommand},
		{"no node", []AlertVerdict{{LocalID: 1, Verdict: store.VerdictConfirmed}}, ErrInvalidCommand},
	}
	for _, tt := range validation {
		t.Run(tt.name, func(t *testing.T) {
			h.t = t
			h.do(NewResolveUnconfirmedAlerts(tt.verdicts), 3800, tt.want)
		})
	}
}

// TestRecordMonitorHistoryValidation covers the checks of a re-evaluated gap
// (FA-UPT-07, ADR 0024). m1 is UP since 10 s, m2 is paused.
func TestRecordMonitorHistoryValidation(t *testing.T) {
	const tenant = "t1"
	down := func(since int64, inc string) HistoryChange {
		return HistoryChange{SinceMs: since, Status: store.StatusDown, IncidentID: inc, Cause: "timeout", ErrorClass: "timeout"}
	}
	up := func(since int64) HistoryChange { return HistoryChange{SinceMs: since, Status: store.StatusUp} }
	tests := []struct {
		name     string
		monitor  string
		expected store.MonitorStatus
		from, to int64
		changes  []HistoryChange
		want     error
	}{
		{"valid gap", "m1", store.StatusUp, 20000, 60000, []HistoryChange{down(30000, "g1"), up(40000)}, nil},
		{"no tenant monitor", "", store.StatusUp, 20000, 60000, []HistoryChange{down(30000, "g1")}, ErrInvalidCommand},
		{"empty interval", "m1", store.StatusUp, 20000, 20000, []HistoryChange{down(20000, "g1")}, ErrInvalidCommand},
		{"no entries", "m1", store.StatusUp, 20000, 60000, nil, ErrInvalidCommand},
		{"expected PAUSED", "m1", store.StatusPaused, 20000, 60000, []HistoryChange{down(30000, "g1")}, ErrInvalidCommand},
		{"entry before interval", "m1", store.StatusUp, 20000, 60000, []HistoryChange{down(19000, "g1")}, ErrInvalidCommand},
		{"entry at end of interval", "m1", store.StatusUp, 20000, 60000, []HistoryChange{down(60000, "g1")}, ErrInvalidCommand},
		{"not ascending", "m1", store.StatusUp, 20000, 60000, []HistoryChange{down(40000, "g1"), up(30000)}, ErrInvalidCommand},
		{"status PAUSED", "m1", store.StatusUp, 20000, 60000, []HistoryChange{{SinceMs: 30000, Status: store.StatusPaused}}, ErrInvalidCommand},
		{"status MAINTENANCE", "m1", store.StatusUp, 20000, 60000, []HistoryChange{{SinceMs: 30000, Status: store.StatusMaintenance}}, ErrInvalidCommand},
		{"first repeats expected status", "m1", store.StatusUp, 20000, 60000, []HistoryChange{up(30000)}, ErrInvalidCommand},
		{"consecutive repeat", "m1", store.StatusUp, 20000, 60000, []HistoryChange{down(30000, "g1"), {SinceMs: 40000, Status: store.StatusDown, IncidentID: "g2"}}, ErrInvalidCommand},
		{"DOWN without incident", "m1", store.StatusUp, 20000, 60000, []HistoryChange{{SinceMs: 30000, Status: store.StatusDown}}, ErrInvalidCommand},
		{"expected status differs", "m1", store.StatusDown, 20000, 60000, []HistoryChange{up(30000)}, store.ErrConflict},
		{"from before current state", "m1", store.StatusUp, 5000, 60000, []HistoryChange{down(6000, "g1")}, store.ErrConflict},
		{"monitor paused", "m2", store.StatusUp, 20000, 60000, []HistoryChange{down(30000, "g1")}, store.ErrConflict},
		{"unknown monitor", "nope", store.StatusUp, 20000, 60000, []HistoryChange{down(30000, "g1")}, store.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := flapSetup(t, 5)
			h.changeState(10100, tenant, "m1", store.StatusPending, store.StatusUp, 10000, "")
			paused := monitorDoc("Web m2")
			paused.Paused = true
			cmd, err := NewPutMonitor("monitor-t1-m2", tenant, "m2", paused, 0)
			if err != nil {
				t.Fatal(err)
			}
			h.do(cmd, 10300, nil)
			h.do(NewRecordMonitorHistory(tenant, tt.monitor, tt.expected, tt.from, tt.to, tt.changes), 60000, tt.want)
		})
	}
}

// TestRecordMonitorHistoryEffects covers the stored history, the reevaluated
// incidents, the state and the absence of notifications (FA-UPT-07, ADR 0024).
func TestRecordMonitorHistoryEffects(t *testing.T) {
	t.Run("up gap with DOWN, UP and open DOWN", func(t *testing.T) {
		h := flapSetup(t, 5)
		h.changeState(10100, "t1", "m1", store.StatusPending, store.StatusUp, 10000, "")
		h.do(NewRecordMonitorHistory("t1", "m1", store.StatusUp, 20000, 60000, []HistoryChange{
			{SinceMs: 30000, Status: store.StatusDown, IncidentID: "g1", Cause: "timeout", ErrorClass: "timeout"},
			{SinceMs: 40000, Status: store.StatusUp},
			{SinceMs: 50000, Status: store.StatusDown, IncidentID: "g2", Cause: "refused", ErrorClass: "connect"},
		}), 60100, nil)

		all, err := h.st.StatusHistory(h.ctx, "t1", "m1", 20000, 60000)
		if err != nil {
			t.Fatal(err)
		}
		// StatusHistory also returns the change before the interval; keep the gap only.
		var changes []store.StatusChange
		for _, c := range all {
			if c.SinceMs >= 20000 {
				changes = append(changes, c)
			}
		}
		wantChanges := []store.StatusChange{{SinceMs: 30000, Status: store.StatusDown}, {SinceMs: 40000, Status: store.StatusUp}, {SinceMs: 50000, Status: store.StatusDown}}
		if !slices.Equal(changes, wantChanges) {
			t.Errorf("history = %v, want %v", changes, wantChanges)
		}

		g1, err := h.st.GetIncident(h.ctx, "t1", "g1")
		if err != nil {
			t.Fatal(err)
		}
		if g1.StartedMs != 30000 || g1.ResolvedMs != 40000 || !g1.Reevaluated || g1.Cause != "timeout" {
			t.Errorf("closed incident g1 = %+v", g1)
		}
		g2, err := h.st.GetIncident(h.ctx, "t1", "g2")
		if err != nil {
			t.Fatal(err)
		}
		if g2.StartedMs != 50000 || g2.ResolvedMs != 0 || !g2.Reevaluated {
			t.Errorf("open incident g2 = %+v", g2)
		}

		if st := h.monitorState("m1"); st.Status != store.StatusDown || st.SinceMs != 50000 || !st.Confirmed {
			t.Errorf("state = %+v, want DOWN since 50000 confirmed", st)
		}
		h.noAlert("m1:30000:DOWN", "a")
		h.noAlert("m1:40000:UP", "a")
		h.noAlert("m1:50000:DOWN", "a")
	})

	t.Run("gap after DOWN resolves the open incident", func(t *testing.T) {
		h := flapSetup(t, 5)
		h.changeState(10100, "t1", "m1", store.StatusPending, store.StatusDown, 10000, "i0")
		h.do(NewRecordMonitorHistory("t1", "m1", store.StatusDown, 20000, 60000, []HistoryChange{
			{SinceMs: 30000, Status: store.StatusUp},
		}), 60100, nil)

		i0, err := h.st.GetIncident(h.ctx, "t1", "i0")
		if err != nil {
			t.Fatal(err)
		}
		if i0.ResolvedMs != 30000 || i0.Reevaluated {
			t.Errorf("incident i0 = %+v, want resolved at 30000 and not reevaluated", i0)
		}
		if st := h.monitorState("m1"); st.Status != store.StatusUp || st.SinceMs != 30000 {
			t.Errorf("state = %+v, want UP since 30000", st)
		}
		h.noAlert("m1:30000:UP", "a")
	})
}

// TestLastIssuedAtFromLevel5 covers ADR 0024: the command time of the last
// applied command is stored from level 5 on, and only by applied commands.
func TestLastIssuedAtFromLevel5(t *testing.T) {
	ctx := context.Background()

	h4 := flapSetup(t, 4)
	h4.putTenant(2500, "t2")
	if got, err := h4.st.LastIssuedAt(ctx); err != nil || got != 0 {
		t.Errorf("level 4: LastIssuedAt = %d, %v; want 0", got, err)
	}

	h5 := flapSetup(t, 5)
	h5.putTenant(2500, "t2")
	if got, err := h5.st.LastIssuedAt(ctx); err != nil || got != 2500 {
		t.Fatalf("level 5: LastIssuedAt = %d, %v; want 2500", got, err)
	}
	// A tenant without ID is rejected, so the command time must not move.
	bad, err := NewPutTenant("bad-tenant", "", TenantDocument{Name: "x", Plan: "default"})
	if err != nil {
		t.Fatal(err)
	}
	h5.do(bad, 3000, ErrInvalidCommand)
	if got, err := h5.st.LastIssuedAt(ctx); err != nil || got != 2500 {
		t.Errorf("rejected command changed LastIssuedAt to %d, %v; want 2500", got, err)
	}
}

// TestDegradedDeterminism: the same commands applied to two fresh stores give
// the same fingerprint (FA-CLU-07, ADR 0024).
func TestDegradedDeterminism(t *testing.T) {
	// The commands are built once: the request ID of a verdict is random, and
	// in a cluster every replica applies the same bytes (request_dedup is hashed).
	verdict := func(v string) *pb.Command {
		return NewResolveUnconfirmedAlerts([]AlertVerdict{{NodeID: "n2", LocalID: 1, Verdict: v, Notify: true}})
	}
	report := NewReportDegradedAlerts("n2", []store.DegradedAlert{reportAlert(1, "m1", "a", store.StatusUp, 15000)})
	history := NewRecordMonitorHistory("t1", "m1", store.StatusDown, 30000, 60000, []HistoryChange{
		{SinceMs: 40000, Status: store.StatusUp},
	})
	confirmed, falseAlarm := verdict(store.VerdictConfirmed), verdict(store.VerdictFalseAlarm)

	run := func(t *testing.T, resolve *pb.Command) string {
		h := newAlertFSM(t, t.TempDir())
		h.setLevel(5)
		h.putTenant(1000, "t1")
		h.putChannel(1000, "t1", "a", channel(allEvents...), nil)
		h.putMonitor(1000, "t1", "m1", "a")
		h.changeState(10100, "t1", "m1", store.StatusPending, store.StatusUp, 10000, "")
		h.changeState(20100, "t1", "m1", store.StatusUp, store.StatusDown, 20000, "i1")
		h.do(report, 25000, nil)
		h.do(resolve, 26000, nil)
		h.do(history, 61000, nil)
		fp, err := h.st.Fingerprint(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return fp
	}
	a := run(t, confirmed)
	if b := run(t, confirmed); a != b {
		t.Fatalf("same commands, different fingerprints: %s != %s", a, b)
	}
	if c := run(t, falseAlarm); c == a {
		t.Error("different verdict gave the same fingerprint")
	}
}

// TestSetMonitorStateCarriesReporters: the leader's reporters and sender reach
// the command; the FSM reads them from level 5 on (ADR 0024).
func TestSetMonitorStateCarriesReporters(t *testing.T) {
	cmd := NewSetMonitorState(StateChange{TenantID: "t1", MonitorID: "m1", From: store.StatusUp, To: store.StatusDown,
		SinceMs: 1, Incident: &store.Incident{ID: "i"}, Reporters: []string{"n1", "n2"}, SenderNodeID: "n1"})
	set := cmd.GetSetMonitorState()
	if fmt.Sprint(set.GetReporters()) != "[n1 n2]" || set.GetSenderNodeId() != "n1" {
		t.Errorf("reporters = %v, sender = %q", set.GetReporters(), set.GetSenderNodeId())
	}
}

// unconfirmed reads a stored unconfirmed alert in a transaction.
func (h *alertFSM) unconfirmed(nodeID string, localID int64) (store.UnconfirmedAlert, error) {
	h.t.Helper()
	var a store.UnconfirmedAlert
	err := h.st.Update(h.ctx, func(tx store.StateTx) error {
		var err error
		a, err = tx.GetUnconfirmedAlert(h.ctx, nodeID, localID)
		return err
	})
	return a, err
}

// TestStateChangeSenderView: from level 5 on, a state-change alert carries the
// proposer, the voter counts of the replicated registry and the reporters
// (FA-ALR-11); below it the view is absent.
func TestStateChangeSenderView(t *testing.T) {
	for _, level := range []uint32{4, 5} {
		t.Run(fmt.Sprintf("level %d", level), func(t *testing.T) {
			h := flapSetup(t, level)
			for i, n := range []store.Node{
				{ID: "n1", Role: store.NodeRoleVoter}, {ID: "n2", Role: store.NodeRoleVoter},
				{ID: "n3", Role: store.NodeRoleVoter}, {ID: "p1", Role: store.NodeRoleProbe},
			} {
				h.do(NewRegisterNode(fmt.Sprintf("reg-%d", i), n), 2000, nil)
			}
			h.do(NewSetNodeHealth("health-n3", "n3", false), 2100, nil)
			h.do(NewSetMonitorState(StateChange{TenantID: "t1", MonitorID: "m1", From: store.StatusPending, To: store.StatusDown,
				SinceMs: 10000, Incident: &store.Incident{ID: "i1"}, Reporters: []string{"n1", "n2"}, SenderNodeID: "n1"}), 10100, nil)

			p := verdictPayload(t, h.alert(alertKey("m1", 10000, store.StatusDown), "a"))
			if level < DegradedCommandLevel {
				if p.Sender != nil {
					t.Errorf("sender view below level 5: %+v", p.Sender)
				}
				return
			}
			want := AlertSender{NodeID: "n1", ReachableVoters: 2, TotalVoters: 3, Reporters: []string{"n1", "n2"}, Confirmed: true}
			if p.Sender == nil || fmt.Sprint(*p.Sender) != fmt.Sprint(want) {
				t.Errorf("sender view = %+v, want %+v", p.Sender, want)
			}
		})
	}
}
