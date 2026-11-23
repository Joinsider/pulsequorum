// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"fmt"
	"testing"

	"github.com/joinsider/pulsequorum/internal/store"
)

// allEvents subscribes a channel to every event of command level 4.
var allEvents = []string{store.EventDown, store.EventUp, store.EventDegraded, store.EventPartial,
	store.EventFlapping, store.EventChannelFailed}

// setLevel stores the command level (ADR 0018) through a command.
func (h *alertFSM) setLevel(level uint32) {
	h.t.Helper()
	h.do(NewSetFeatureLevel(fmt.Sprintf("level-%d", level), level), 1000, nil)
}

// monitorState returns the state of monitor id of tenant t1.
func (h *alertFSM) monitorState(id string) store.MonitorState {
	h.t.Helper()
	m, err := h.st.GetMonitor(h.ctx, "t1", id)
	if err != nil {
		h.t.Fatal(err)
	}
	return m.State
}

// flapSetup creates tenant t1 with channel a and monitor m1 subscribed to a.
func flapSetup(t *testing.T, level uint32) *alertFSM {
	t.Helper()
	h := newAlertFSM(t, t.TempDir())
	h.setLevel(level)
	h.putTenant(1000, "t1")
	h.putChannel(1000, "t1", "a", channel(allEvents...), nil)
	h.putMonitor(1000, "t1", "m1", "a")
	return h
}

// flapChanges applies alternating changes of m1 at 10 s steps and returns the
// command time of the last one. The first change starts from PENDING.
func flapChanges(h *alertFSM, statuses ...store.MonitorStatus) int64 {
	h.t.Helper()
	from := store.StatusPending
	var since int64
	for i, to := range statuses {
		since = 10_000 + int64(i)*10_000
		var inc string
		if to == store.StatusDown {
			inc = "i-" + string(rune('a'+i))
		}
		h.changeState(since+100, "t1", "m1", from, to, since, inc)
		from = to
	}
	return since
}

// TestFlappingStartsAtSixthChange covers FA-ALR-06: five changes notify
// normally, the sixth starts flapping with one summary and no normal alert.
func TestFlappingStartsAtSixthChange(t *testing.T) {
	t.Parallel()
	h := flapSetup(t, 4)
	// UP(10s, silent), DOWN, UP, DOWN, UP: five counted changes.
	flapChanges(h, store.StatusUp, store.StatusDown, store.StatusUp, store.StatusDown, store.StatusUp)
	h.alert("m1:20000:DOWN", "a")
	h.alert("m1:30000:UP", "a")
	h.alert("m1:50000:UP", "a")
	if st := h.monitorState("m1"); st.Flapping {
		t.Fatalf("flapping after five changes: %+v", st)
	}

	// The sixth change (DOWN at 60 s) makes six within 30 min.
	h.changeState(60_100, "t1", "m1", store.StatusUp, store.StatusDown, 60_000, "i-six")
	h.noAlert("m1:60000:DOWN", "a")
	h.checkPayload(h.alert("m1:60100:FLAPPING", "a"), map[string]any{
		"event": "flapping", "flapping": true, "changeCount": 6.0, "status": "DOWN", "monitorId": "m1", "sinceMs": 60_000.0,
	})
	st := h.monitorState("m1")
	if !st.Flapping || st.FlappingSinceMs != 60_100 || st.Status != store.StatusDown {
		t.Errorf("state = %+v, want flapping since 60100 in DOWN", st)
	}
}

// TestFlappingSuppressesAlertsAndReminders covers FA-ALR-06: while flapping no
// change and no reminder produces an alert, but the status is still written.
func TestFlappingSuppressesAlertsAndReminders(t *testing.T) {
	t.Parallel()
	h := flapSetup(t, 4)
	flapChanges(h, store.StatusUp, store.StatusDown, store.StatusUp, store.StatusDown, store.StatusUp, store.StatusDown)
	// Flapping since 60100 (the sixth change at 60 s).
	h.changeState(70_100, "t1", "m1", store.StatusDown, store.StatusUp, 70_000, "")
	h.changeState(80_100, "t1", "m1", store.StatusUp, store.StatusDown, 80_000, "i-seven")
	h.noAlert("m1:70000:UP", "a")
	h.noAlert("m1:80000:DOWN", "a")
	st := h.monitorState("m1")
	if st.Status != store.StatusDown || st.SinceMs != 80_000 || !st.Flapping {
		t.Errorf("state = %+v, want DOWN since 80000 while flapping", st)
	}
	if inc, err := h.st.GetIncident(h.ctx, "t1", "i-seven"); err != nil || inc.MonitorID != "m1" {
		t.Errorf("incident while flapping: %+v, %v; want the incident to open", inc, err)
	}

	// Reminder of the DOWN period: suppressed while flapping.
	h.do(NewEnqueueReminder("t1", "m1", 80_000, 1), 90_000, nil)
	h.noAlert("m1:80000:DOWN:reminder:1", "a")
}

// TestClearMonitorFlapping covers FA-ALR-06: the leader ends the period with
// the start it saw; another start is a conflict.
func TestClearMonitorFlapping(t *testing.T) {
	t.Parallel()
	h := flapSetup(t, 4)
	flapChanges(h, store.StatusUp, store.StatusDown, store.StatusUp, store.StatusDown, store.StatusUp, store.StatusDown)
	h.changeState(70_100, "t1", "m1", store.StatusDown, store.StatusUp, 70_000, "")

	h.do(NewClearMonitorFlapping("t1", "m1", 12_345), 200_000, store.ErrConflict)
	if st := h.monitorState("m1"); !st.Flapping {
		t.Fatal("wrong since cleared the flapping period")
	}

	// The leader clears after 30 min without a change (last change at 70 s).
	h.do(NewClearMonitorFlapping("t1", "m1", 60_100), 1_900_000, nil)
	h.checkPayload(h.alert("m1:60100:FLAPPING:END", "a"), map[string]any{
		"event": "flapping", "changeCount": 1.0, "status": "UP", "sinceMs": 70_000.0,
	})
	if st := h.monitorState("m1"); st.Flapping || st.FlappingSinceMs != 0 {
		t.Errorf("state after clear = %+v, want not flapping", st)
	}

	// Once cleared, a change notifies normally again: the window of 30 min holds
	// no earlier change.
	h.changeState(1_930_100, "t1", "m1", store.StatusUp, store.StatusDown, 1_930_000, "i-again")
	h.alert("m1:1930000:DOWN", "a")
	// The same clear is a duplicate and returns its first result; another start is a conflict.
	h.do(NewClearMonitorFlapping("t1", "m1", 60_100), 1_940_000, nil)
	h.do(NewClearMonitorFlapping("t1", "m1", 1_930_000), 1_940_000, store.ErrConflict)
}

// TestFlappingNeedsLevelFour covers ADR 0023: below command level 4 the FSM
// behaves as before, and the new commands and events are rejected.
func TestFlappingNeedsLevelFour(t *testing.T) {
	t.Parallel()
	for _, level := range []uint32{0, 3} {
		t.Run(fmt.Sprintf("stored level %d", level), func(t *testing.T) {
			t.Parallel()
			h := newAlertFSM(t, t.TempDir())
			if level != 0 {
				h.setLevel(level)
			}
			h.putTenant(1000, "t1")
			h.putChannel(1000, "t1", "a", channel(store.EventDown, store.EventUp), nil)
			h.putMonitor(1000, "t1", "m1", "a")
			flapChanges(h, store.StatusUp, store.StatusDown, store.StatusUp, store.StatusDown, store.StatusUp,
				store.StatusDown, store.StatusUp, store.StatusDown)
			st := h.monitorState("m1")
			if st.Flapping {
				t.Fatalf("level %d: flapping started: %+v", level, st)
			}
			// Every change notifies, as before: the sixth change (DOWN at 60 s) too.
			h.alert("m1:60000:DOWN", "a")
			h.alert("m1:70000:UP", "a")
			h.alert("m1:80000:DOWN", "a")
			h.do(NewClearMonitorFlapping("t1", "m1", 60_100), 200_000, ErrInvalidCommand)
			h.do(NewEnqueueReminder("t1", "m1", 80_000, 1), 90_000, nil)
			h.alert("m1:80000:DOWN:reminder:1", "a")
		})
	}

	// Channels cannot subscribe to the new events below level 4.
	h := newAlertFSM(t, t.TempDir())
	h.setLevel(3)
	h.putTenant(1000, "t1")
	h.putChannel(1000, "t1", "x", channel(store.EventFlapping), ErrInvalidCommand)
	h.putChannel(1000, "t1", "y", channel(store.EventChannelFailed), ErrInvalidCommand)
	h.setLevel(4)
	h.putChannel(1000, "t1", "z", channel(store.EventFlapping), nil)
}

// TestChannelFailedAlert covers FA-ALR-05: a channel that turns disabled or
// failing alerts the other default channels that subscribe to channel_failed.
func TestChannelFailedAlert(t *testing.T) {
	t.Parallel()
	h := newAlertFSM(t, t.TempDir())
	h.setLevel(4)
	h.putTenant(1000, "t1")
	h.putChannel(1000, "t1", "bad", ChannelDocument{Name: "bad", Kind: store.ChannelWebhook, URLEnc: encURL, Events: allEvents, IsDefault: true}, nil)
	h.putChannel(1000, "t1", "ok1", ChannelDocument{Name: "ok1", Kind: store.ChannelWebhook, URLEnc: encURL, Events: []string{store.EventChannelFailed}, IsDefault: true}, nil)
	h.putChannel(1000, "t1", "ok2", ChannelDocument{Name: "ok2", Kind: store.ChannelWebhook, URLEnc: encURL, Events: []string{store.EventChannelFailed}, IsDefault: true}, nil)
	h.putChannel(1000, "t1", "plain", ChannelDocument{Name: "plain", Kind: store.ChannelWebhook, URLEnc: encURL, Events: []string{store.EventChannelFailed}}, nil)
	h.putChannel(1000, "t1", "quiet", ChannelDocument{Name: "quiet", Kind: store.ChannelWebhook, URLEnc: encURL, Events: []string{store.EventDown}, IsDefault: true}, nil)
	h.putChannel(1000, "t1", "gone", ChannelDocument{Name: "gone", Kind: store.ChannelWebhook, URLEnc: encURL, Events: allEvents, IsDefault: true}, nil)

	// "gone" is disabled first: its own failure notifies ok1 and ok2 once.
	h.putMonitor(1000, "t1", "m0", "gone")
	h.changeState(1100, "t1", "m0", store.StatusPending, store.StatusDown, 1000, "i0")
	h.ack(1200, "t1", "m0:1000:DOWN", "gone", 1, DeliveryOutcome{Permanent: true, Error: "401"}, nil)
	h.alert("channel:gone:disabled:1200", "ok1")
	h.alert("channel:gone:disabled:1200", "ok2")

	// "bad" is disabled: ok1 and ok2 are told, the disabled and the
	// non-default channels are not.
	h.putMonitor(1300, "t1", "m1", "bad")
	h.changeState(1400, "t1", "m1", store.StatusPending, store.StatusDown, 1300, "i1")
	h.ack(1500, "t1", "m1:1300:DOWN", "bad", 1, DeliveryOutcome{Permanent: true, Error: "401"}, nil)
	key := "channel:bad:disabled:1500"
	for _, ch := range []string{"ok1", "ok2"} {
		a := h.alert(key, ch)
		if a.MonitorID != "m1" || a.NextAttemptMs != 1500 {
			t.Errorf("entry %s = %+v", ch, a)
		}
		h.checkPayload(a, map[string]any{"event": "channel_failed", "channelId": "bad", "channelName": "bad",
			"channelError": "401", "monitorId": "m1"})
	}
	h.noAlert(key, "bad")
	h.noAlert(key, "plain")
	h.noAlert(key, "quiet")
	h.noAlert(key, "gone")

	// A second failing alert of "failing" once: channel "flaky" gives up for
	// two monitors; only the first state change notifies.
	h.putChannel(1000, "t1", "flaky", ChannelDocument{Name: "flaky", Kind: store.ChannelWebhook, URLEnc: encURL, Events: allEvents, IsDefault: true}, nil)
	h.putMonitor(1600, "t1", "m2", "flaky")
	h.putMonitor(1600, "t1", "m3", "flaky")
	h.changeState(1700, "t1", "m2", store.StatusPending, store.StatusDown, 1650, "i2")
	h.changeState(1700, "t1", "m3", store.StatusPending, store.StatusDown, 1650, "i3")
	h.ack(1800, "t1", "m2:1650:DOWN", "flaky", 1, DeliveryOutcome{GiveUp: true, Error: "timeout"}, nil)
	h.ack(1900, "t1", "m3:1650:DOWN", "flaky", 1, DeliveryOutcome{GiveUp: true, Error: "timeout"}, nil)
	h.alert("channel:flaky:failing:1800", "ok1")
	h.noAlert("channel:flaky:failing:1900", "ok1")
	if c := h.channelOf("t1", "flaky"); c.State != store.ChannelFailing {
		t.Errorf("flaky = %+v, want failing", c)
	}

	// Recovery is not a failure: no channel_failed alert.
	h.putMonitor(2000, "t1", "m4", "flaky")
	h.changeState(2100, "t1", "m4", store.StatusPending, store.StatusDown, 2050, "i4")
	h.ack(2200, "t1", "m4:2050:DOWN", "flaky", 1, DeliveryOutcome{Delivered: true}, nil)
	h.noAlert("channel:flaky:ok:2200", "ok1")
	if c := h.channelOf("t1", "flaky"); c.State != store.ChannelOK {
		t.Errorf("flaky = %+v, want ok after delivery", c)
	}
}

// TestFlappingSurvivesPause: pausing and resuming keep the flapping period,
// because they write the state without the flapping fields.
func TestFlappingSurvivesPause(t *testing.T) {
	t.Parallel()
	h := flapSetup(t, 4)
	flapChanges(h, store.StatusUp, store.StatusDown, store.StatusUp, store.StatusDown, store.StatusUp, store.StatusDown)
	if !h.monitorState("m1").Flapping {
		t.Fatal("monitor does not flap")
	}
	doc := monitorDoc("Web m1")
	doc.NotificationChannelIDs = []string{"a"}
	doc.Paused = true
	cmd, err := NewPutMonitor("pause-m1", "t1", "m1", doc, 1)
	if err != nil {
		t.Fatal(err)
	}
	h.do(cmd, 90_000, nil)
	if st := h.monitorState("m1"); st.Status != store.StatusPaused || !st.Flapping || st.FlappingSinceMs != 60_100 {
		t.Errorf("paused state = %+v, want PAUSED and still flapping since 60100", st)
	}
}

// TestFlappingFingerprint: the flapping columns are part of the replicated
// state, so replicas that flapped alike have the same fingerprint, and a
// different flapping state a different one (ADR 0005, FA-CLU-07).
func TestFlappingFingerprint(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T) *alertFSM {
		h := flapSetup(t, 4)
		flapChanges(h, store.StatusUp, store.StatusDown, store.StatusUp, store.StatusDown, store.StatusUp, store.StatusDown)
		return h
	}
	a, b := run(t), run(t)
	fpA, err := a.st.Fingerprint(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fpB, _ := b.st.Fingerprint(b.ctx); fpA != fpB {
		t.Fatalf("same history, different fingerprints: %s != %s", fpA, fpB)
	}
	if err := a.st.Update(a.ctx, func(tx store.StateTx) error {
		return tx.SetMonitorFlapping(a.ctx, "t1", "m1", false, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if fpA2, _ := a.st.Fingerprint(a.ctx); fpA2 == fpA {
		t.Error("clearing the flapping flag did not change the fingerprint")
	}
}
