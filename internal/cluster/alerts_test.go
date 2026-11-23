// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

// encURL is a channel URL as the leader proposes it: encrypted (ADR 0011).
const encURL = "enc:v1:key:AAAA"

// alertFSM drives an FSM with increasing log indexes and reads its store.
type alertFSM struct {
	t   *testing.T
	ctx context.Context
	st  *sqlite.StateStore
	fsm *FSM
	idx uint64
}

func newAlertFSM(t *testing.T, dir string) *alertFSM {
	t.Helper()
	ctx := context.Background()
	st := openStore(t, dir)
	fsm, err := NewFSM(ctx, st, slog.New(slog.DiscardHandler), func(err error) { t.Errorf("unexpected halt: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	return &alertFSM{t: t, ctx: ctx, st: st, fsm: fsm}
}

// apply runs cmd at the next log index with the given command time.
func (h *alertFSM) apply(cmd *pb.Command, issuedAt int64) ApplyResult {
	h.t.Helper()
	cmd.IssuedAtMs = issuedAt
	data, err := proto.Marshal(cmd)
	if err != nil {
		h.t.Fatal(err)
	}
	h.idx++
	return h.fsm.Apply(&raft.Log{Index: h.idx, Type: raft.LogCommand, Data: data}).(ApplyResult)
}

// do applies cmd and checks its outcome; want nil means success.
func (h *alertFSM) do(cmd *pb.Command, issuedAt int64, want error) {
	h.t.Helper()
	res := h.apply(cmd, issuedAt)
	if !errors.Is(res.Err, want) {
		h.t.Fatalf("command %q: err = %v, want %v", cmd.GetRequestId(), res.Err, want)
	}
}

func (h *alertFSM) putTenant(issuedAt int64, id string) {
	h.t.Helper()
	cmd, err := NewPutTenant("tenant-"+id, id, TenantDocument{Name: id, Plan: "default"})
	if err != nil {
		h.t.Fatal(err)
	}
	h.do(cmd, issuedAt, nil)
}

func (h *alertFSM) putChannel(issuedAt int64, tenantID, id string, doc ChannelDocument, want error) {
	h.t.Helper()
	cmd, err := NewPutChannel("channel-"+tenantID+"-"+id, tenantID, id, doc, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	h.do(cmd, issuedAt, want)
}

// channel returns a valid channel document subscribed to the given events.
func channel(events ...string) ChannelDocument {
	return ChannelDocument{Name: "channel", Kind: store.ChannelWebhook, URLEnc: encURL, Events: events}
}

// putMonitor creates a monitor in state PENDING, subscribed to the channels.
func (h *alertFSM) putMonitor(issuedAt int64, tenantID, id string, channelIDs ...string) {
	h.t.Helper()
	doc := monitorDoc("Web " + id)
	doc.NotificationChannelIDs = channelIDs
	cmd, err := NewPutMonitor("monitor-"+tenantID+"-"+id, tenantID, id, doc, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	h.do(cmd, issuedAt, nil)
}

// changeState applies a status change; incidentID is required for DOWN.
func (h *alertFSM) changeState(issuedAt int64, tenantID, monitorID string, from, to store.MonitorStatus, sinceMs int64, incidentID string) {
	h.t.Helper()
	c := StateChange{TenantID: tenantID, MonitorID: monitorID, From: from, To: to, SinceMs: sinceMs}
	if incidentID != "" {
		c.Incident = &store.Incident{ID: incidentID, Cause: "connection refused", ErrorClass: "connect", AffectedNodes: []string{"n1"}}
	}
	h.do(NewSetMonitorState(c), issuedAt, nil)
}

// ack records a delivery attempt of the outbox entry key/channelID as tenantID.
func (h *alertFSM) ack(issuedAt int64, tenantID, key, channelID string, attempt uint32, o DeliveryOutcome, want error) {
	h.t.Helper()
	h.do(NewAckAlertDelivery(store.AlertDelivery{TenantID: tenantID, AlertKey: key, ChannelID: channelID}, attempt, o), issuedAt, want)
}

func (h *alertFSM) alert(key, channelID string) store.AlertDelivery {
	h.t.Helper()
	a, err := h.st.GetAlert(h.ctx, key, channelID)
	if err != nil {
		h.t.Fatalf("GetAlert(%s, %s): %v", key, channelID, err)
	}
	return a
}

func (h *alertFSM) noAlert(key, channelID string) {
	h.t.Helper()
	if _, err := h.st.GetAlert(h.ctx, key, channelID); !errors.Is(err, store.ErrNotFound) {
		h.t.Errorf("GetAlert(%s, %s) error = %v, want ErrNotFound", key, channelID, err)
	}
}

func (h *alertFSM) channelOf(tenantID, id string) store.NotificationChannel {
	h.t.Helper()
	c, err := h.st.GetChannel(h.ctx, tenantID, id)
	if err != nil {
		h.t.Fatalf("GetChannel(%s, %s): %v", tenantID, id, err)
	}
	return c
}

// checkPayload compares the JSON fields of an outbox entry.
func (h *alertFSM) checkPayload(a store.AlertDelivery, want map[string]any) {
	h.t.Helper()
	var got map[string]any
	if err := json.Unmarshal(a.Payload, &got); err != nil {
		h.t.Fatalf("payload of %s/%s: %v", a.AlertKey, a.ChannelID, err)
	}
	for k, v := range want {
		if got[k] != v {
			h.t.Errorf("payload of %s/%s: %s = %v, want %v", a.AlertKey, a.ChannelID, k, got[k], v)
		}
	}
}

func TestAlertEvent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		from, to store.MonitorStatus
		want     string
	}{
		{"down from up", store.StatusUp, store.StatusDown, store.EventDown},
		{"down from pending", store.StatusPending, store.StatusDown, store.EventDown},
		{"up after down", store.StatusDown, store.StatusUp, store.EventUp},
		{"first up is silent", store.StatusPending, store.StatusUp, ""},
		{"pending is silent", store.StatusUp, store.StatusPending, ""},
		{"pending after down is silent", store.StatusDown, store.StatusPending, ""},
		{"paused is silent", store.StatusUp, store.StatusPaused, ""},
		{"unknown is silent", store.StatusUp, store.StatusUnknown, ""},
		{"degraded after down is up", store.StatusDown, store.StatusDegraded, store.EventUp},
		{"degraded from up", store.StatusUp, store.StatusDegraded, store.EventDegraded},
		{"degraded from pending", store.StatusPending, store.StatusDegraded, store.EventDegraded},
		{"partial from up", store.StatusUp, store.StatusPartial, store.EventPartial},
		{"partial from down", store.StatusDown, store.StatusPartial, store.EventPartial},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := alertEvent(tt.from, tt.to); got != tt.want {
				t.Errorf("alertEvent(%s, %s) = %q, want %q", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

// TestChannelValidation covers FA-ALR-01: secrets arrive encrypted, kinds and
// events are known, and every channel belongs to a tenant.
func TestChannelValidation(t *testing.T) {
	t.Parallel()
	plainHeader := channel(store.EventDown)
	plainHeader.WebhookHeaders = map[string]string{"Authorization": "Bearer abc"}
	encHeader := channel(store.EventDown)
	encHeader.WebhookHeaders = map[string]string{"Authorization": "enc:v1:key:BBBB"}
	plainURL := channel(store.EventDown)
	plainURL.URLEnc = "https://hooks.example.test/abc"
	badKind := channel(store.EventDown)
	badKind.Kind = "email"
	badEvent := channel("sms")

	tests := []struct {
		name   string
		tenant string
		id     string
		doc    ChannelDocument
		want   error
	}{
		{"plaintext URL", "t1", "c-url", plainURL, ErrInvalidCommand},
		{"plaintext header", "t1", "c-header", plainHeader, ErrInvalidCommand},
		{"unknown kind", "t1", "c-kind", badKind, ErrInvalidCommand},
		{"unknown event", "t1", "c-event", badEvent, ErrInvalidCommand},
		{"without tenant", "", "c-tenant", channel(store.EventDown), ErrInvalidCommand},
		{"valid with encrypted header", "t1", "c-ok", encHeader, nil},
	}
	h := newAlertFSM(t, t.TempDir())
	h.putTenant(1000, "t1")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := NewPutChannel("channel-"+tt.id, tt.tenant, tt.id, tt.doc, 0)
			if err != nil {
				t.Fatal(err)
			}
			res := h.apply(cmd, 1000)
			if !errors.Is(res.Err, tt.want) {
				t.Fatalf("err = %v, want %v", res.Err, tt.want)
			}
			if tt.want != nil {
				if _, err := h.st.GetChannel(h.ctx, tt.tenant, tt.id); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("rejected channel stored: %v", err)
				}
			}
		})
	}
}

// TestStateChangeAlerts covers FA-ALR-02: a status change enqueues one entry
// per enabled channel that subscribes to its event, in the same transaction.
func TestStateChangeAlerts(t *testing.T) {
	t.Parallel()
	h := newAlertFSM(t, t.TempDir())
	h.putTenant(1000, "t1")
	h.putChannel(1000, "t1", "a", channel(store.EventDown, store.EventUp), nil)
	h.putChannel(1000, "t1", "b", channel(store.EventUp), nil)
	h.putChannel(1000, "t1", "c", channel(store.EventDown), nil)

	// Channel c is disabled by a permanent failure of its own alert.
	h.putMonitor(1000, "t1", "m0", "c")
	h.changeState(1600, "t1", "m0", store.StatusPending, store.StatusDown, 1500, "i0")
	h.ack(1700, "t1", "m0:1500:DOWN", "c", 1, DeliveryOutcome{Permanent: true, Error: "401 unauthorized"}, nil)
	if c := h.channelOf("t1", "c"); c.State != store.ChannelDisabled || c.LastError != "401 unauthorized" {
		t.Fatalf("channel c = %+v, want disabled", c)
	}

	h.putMonitor(1800, "t1", "m1", "a", "b", "c")
	h.changeState(2100, "t1", "m1", store.StatusPending, store.StatusDown, 2000, "i1")
	down := "m1:2000:DOWN"
	a := h.alert(down, "a")
	if a.State != store.AlertPending || a.Attempts != 0 || a.NextAttemptMs != 2100 || a.MonitorID != "m1" {
		t.Errorf("entry for a = %+v", a)
	}
	h.checkPayload(a, map[string]any{
		"alertId": down, "tenantId": "t1", "monitorId": "m1", "monitorName": "Web m1", "monitorType": "http",
		"event": "down", "status": "DOWN", "previousStatus": "PENDING", "sinceMs": 2000.0,
		"cause": "connection refused", "errorClass": "connect", "incidentId": "i1", "confirmed": true,
	})
	h.noAlert(down, "b")
	h.noAlert(down, "c")

	// Recovery is announced to every subscriber of up, but not to c.
	h.changeState(3100, "t1", "m1", store.StatusDown, store.StatusUp, 3000, "")
	up := "m1:3000:UP"
	for _, ch := range []string{"a", "b"} {
		e := h.alert(up, ch)
		if e.NextAttemptMs != 3100 {
			t.Errorf("entry %s next attempt = %d, want 3100", ch, e.NextAttemptMs)
		}
		h.checkPayload(e, map[string]any{"event": "up", "status": "UP", "previousStatus": "DOWN", "sinceMs": 3000.0})
	}
	h.noAlert(up, "c")

	// The first UP of a new monitor is silent.
	h.putMonitor(3200, "t1", "m2", "a", "b")
	h.changeState(3400, "t1", "m2", store.StatusPending, store.StatusUp, 3300, "")
	h.noAlert("m2:3300:UP", "a")
	h.noAlert("m2:3300:UP", "b")
}

// TestEnqueueReminder covers FA-ALR-02: repeated DOWN alerts belong to one
// DOWN period and stop when the monitor recovers.
func TestEnqueueReminder(t *testing.T) {
	t.Parallel()
	h := newAlertFSM(t, t.TempDir())
	h.putTenant(1000, "t1")
	h.putChannel(1000, "t1", "a", channel(store.EventDown), nil)
	h.putChannel(1000, "t1", "b", channel(store.EventUp), nil)
	h.putMonitor(1000, "t1", "m1", "a", "b")
	h.changeState(2100, "t1", "m1", store.StatusPending, store.StatusDown, 2000, "i1")

	h.do(NewEnqueueReminder("t1", "m1", 2000, 1), 2200, nil)
	reminder := "m1:2000:DOWN:reminder:1"
	r := h.alert(reminder, "a")
	h.checkPayload(r, map[string]any{"event": "down", "status": "DOWN", "previousStatus": "DOWN", "sinceMs": 2000.0, "reminder": 1.0})
	if r.NextAttemptMs != 2200 {
		t.Errorf("reminder next attempt = %d, want 2200", r.NextAttemptMs)
	}
	h.noAlert(reminder, "b")

	h.do(NewEnqueueReminder("t1", "m1", 1999, 2), 2300, store.ErrConflict)
	h.do(NewEnqueueReminder("t1", "m1", 2000, 0), 2300, ErrInvalidCommand)

	h.changeState(3100, "t1", "m1", store.StatusDown, store.StatusUp, 3000, "")
	h.do(NewEnqueueReminder("t1", "m1", 2000, 2), 3200, store.ErrConflict)
}

// TestAckAlertDelivery covers FA-ALR-03 and FA-ALR-05: the leader reports each
// attempt, and the outcome decides the entry and the channel state.
func TestAckAlertDelivery(t *testing.T) {
	t.Parallel()
	h := newAlertFSM(t, t.TempDir())
	h.putTenant(1000, "t1")
	h.putTenant(1000, "t2")
	for _, id := range []string{"a", "b", "c", "d", "e", "x"} {
		h.putChannel(1000, "t1", id, channel(store.EventDown), nil)
	}
	h.putMonitor(1000, "t1", "m1", "a", "b", "c", "d", "e", "x")
	h.changeState(2100, "t1", "m1", store.StatusPending, store.StatusDown, 2000, "i1")
	key := "m1:2000:DOWN"

	// Attempt 2 before attempt 1 is out of order.
	h.ack(2150, "t1", key, "a", 2, DeliveryOutcome{Delivered: true}, store.ErrConflict)
	if a := h.alert(key, "a"); a.State != store.AlertPending || a.Attempts != 0 {
		t.Errorf("out of order ack changed entry: %+v", a)
	}
	// Another tenant cannot ack the entry; it stays pending.
	h.ack(2160, "t2", key, "x", 1, DeliveryOutcome{Delivered: true}, store.ErrNotFound)
	if x := h.alert(key, "x"); x.State != store.AlertPending || x.Attempts != 0 {
		t.Errorf("foreign ack changed entry: %+v", x)
	}

	// Delivered: the entry is done and the channel is healthy again.
	if err := h.st.Update(h.ctx, func(tx store.StateTx) error {
		return tx.SetChannelState(h.ctx, "t1", "b", store.ChannelFailing, "earlier failure")
	}); err != nil {
		t.Fatal(err)
	}
	h.ack(2200, "t1", key, "b", 1, DeliveryOutcome{Delivered: true}, nil)
	if b := h.alert(key, "b"); b.State != store.AlertDelivered || b.Attempts != 1 {
		t.Errorf("delivered entry = %+v", b)
	}
	if c := h.channelOf("t1", "b"); c.State != store.ChannelOK || c.LastError != "" {
		t.Errorf("channel b after delivery = %+v, want ok", c)
	}
	h.ack(2210, "t1", key, "b", 2, DeliveryOutcome{Delivered: true}, store.ErrConflict)

	// Temporary failures keep the entry pending; the next attempt is never before the command time.
	h.ack(2300, "t1", key, "c", 1, DeliveryOutcome{Error: "timeout", NextAttemptMs: 2500}, nil)
	if c := h.alert(key, "c"); c.State != store.AlertPending || c.Attempts != 1 || c.NextAttemptMs != 2500 || c.LastError != "timeout" {
		t.Errorf("after temporary failure: %+v", c)
	}
	h.ack(2400, "t1", key, "c", 2, DeliveryOutcome{Error: "timeout", NextAttemptMs: 100}, nil)
	if c := h.alert(key, "c"); c.Attempts != 2 || c.NextAttemptMs != 2400 {
		t.Errorf("next attempt in the past = %d after %d attempts, want 2400 after 2", c.NextAttemptMs, c.Attempts)
	}

	// A permanent failure disables the channel; its entry is failed.
	h.ack(2500, "t1", key, "d", 1, DeliveryOutcome{Permanent: true, Error: "400 bad request"}, nil)
	if d := h.alert(key, "d"); d.State != store.AlertFailed || d.Attempts != 1 {
		t.Errorf("permanently failed entry = %+v", d)
	}
	if d := h.channelOf("t1", "d"); d.State != store.ChannelDisabled || d.LastError != "400 bad request" {
		t.Errorf("channel d = %+v, want disabled", d)
	}

	// Giving up marks the entry failed, but the channel only as failing.
	h.ack(2600, "t1", key, "e", 1, DeliveryOutcome{GiveUp: true, Error: "dns error"}, nil)
	if e := h.alert(key, "e"); e.State != store.AlertFailed {
		t.Errorf("given up entry = %+v", e)
	}
	if e := h.channelOf("t1", "e"); e.State != store.ChannelFailing || e.LastError != "dns error" {
		t.Errorf("channel e = %+v, want failing", e)
	}
	h.ack(2610, "t1", key, "d", 2, DeliveryOutcome{Delivered: true}, store.ErrConflict)

	// A later ack more than 7 days after the finished entries prunes them; an
	// entry finished exactly 7 days earlier is kept.
	const week = 7 * 24 * 3600 * 1000
	h.ack(2600+week, "t1", key, "c", 3, DeliveryOutcome{Delivered: true}, nil)
	h.noAlert(key, "b")
	h.noAlert(key, "d")
	if e := h.alert(key, "e"); e.State != store.AlertFailed {
		t.Errorf("entry at the retention boundary was pruned: %+v", e)
	}
}

// TestDeterministicAlerts applies the same alert history to two replicas and
// compares their state (FA-CLU-07).
func TestDeterministicAlerts(t *testing.T) {
	t.Parallel()
	var prints []string
	for range 2 {
		h := newAlertFSM(t, t.TempDir())
		h.putTenant(1000, "t1")
		h.putChannel(1000, "t1", "a", channel(store.EventDown, store.EventUp), nil)
		h.putChannel(1000, "t1", "b", channel(store.EventDown), nil)
		h.putMonitor(1000, "t1", "m1", "a", "b")
		h.changeState(2100, "t1", "m1", store.StatusPending, store.StatusDown, 2000, "i1")
		h.do(NewEnqueueReminder("t1", "m1", 2000, 1), 2200, nil)
		h.ack(2300, "t1", "m1:2000:DOWN", "a", 1, DeliveryOutcome{Error: "timeout", NextAttemptMs: 2400}, nil)
		h.ack(2500, "t1", "m1:2000:DOWN", "b", 1, DeliveryOutcome{Permanent: true, Error: "gone"}, nil)
		h.changeState(3100, "t1", "m1", store.StatusDown, store.StatusUp, 3000, "")
		h.do(NewEnqueueReminder("t1", "m1", 2000, 2), 3200, store.ErrConflict)

		fp, err := h.st.Fingerprint(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		prints = append(prints, fp)
	}
	if prints[0] != prints[1] {
		t.Fatalf("replicas diverged: %s vs %s", prints[0], prints[1])
	}
}
