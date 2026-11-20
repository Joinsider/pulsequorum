// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package alert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

const (
	testNodeID    = "66666666-6666-6666-6666-666666666666"
	testMonitorID = "m1"
	testChannelID = "ch1"
)

type fixture struct {
	t     *testing.T
	ctx   context.Context
	state *sqlite.StateStore
	node  *cluster.Node
	kr    Keyring
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	dir := t.TempDir()
	state, err := sqlite.OpenState(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	node, err := cluster.Start(ctx, cluster.Config{NodeID: testNodeID, Advertise: "n:9443", DataDir: dir,
		Timeout: 200 * time.Millisecond, Logger: slog.New(slog.DiscardHandler), RaftLogLevel: "error"}, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = state.Close()
	})
	if err := cluster.EnsureDefaultTenant(ctx, node, state); err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, ctx: ctx, state: state, node: node, kr: testKeyring(t)}
}

// channel creates an enabled channel subscribed to down and up.
func (f *fixture) channel(id, kind, rawURL string) {
	f.t.Helper()
	enc, err := EncryptURL(f.kr, id, rawURL)
	if err != nil {
		f.t.Fatal(err)
	}
	cmd, err := cluster.NewPutChannel("", cluster.DefaultTenantID, id, cluster.ChannelDocument{
		Name: id, Kind: kind, URLEnc: enc, Events: []string{store.EventDown, store.EventUp},
	}, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.node.Apply(f.ctx, cmd); err != nil {
		f.t.Fatal(err)
	}
}

// monitor creates testMonitorID subscribed to the given channels.
func (f *fixture) monitor(name string, resendMinutes int, channels ...string) {
	f.t.Helper()
	doc := cluster.MonitorDocument{Name: name, Type: "http", IntervalSeconds: 60, TimeoutSeconds: 10,
		RecoveryThreshold: 1, ResendIntervalMinutes: resendMinutes, Assignment: store.Assignment{Mode: "all"},
		ConsensusPolicy: "majority", NotificationChannelIDs: channels}
	cmd, err := cluster.NewPutMonitor("", cluster.DefaultTenantID, testMonitorID, doc, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.node.Apply(f.ctx, cmd); err != nil {
		f.t.Fatal(err)
	}
}

// setState changes the status of the test monitor through the FSM.
func (f *fixture) setState(from, to store.MonitorStatus, sinceMs int64, incidentID string) {
	f.t.Helper()
	c := cluster.StateChange{TenantID: cluster.DefaultTenantID, MonitorID: testMonitorID, From: from, To: to, SinceMs: sinceMs}
	if to == store.StatusDown {
		c.Incident = &store.Incident{ID: incidentID, Cause: "connection refused", ErrorClass: string(checker.ClassConnect)}
	}
	if _, err := f.node.Apply(f.ctx, cluster.NewSetMonitorState(c)); err != nil {
		f.t.Fatal(err)
	}
}

// pending returns the due outbox entries at nowMs.
func (f *fixture) pending(nowMs int64) []store.AlertDelivery {
	f.t.Helper()
	due, err := f.state.DueAlerts(f.ctx, nowMs, 100)
	if err != nil {
		f.t.Fatal(err)
	}
	return due
}

func (f *fixture) dispatcher(s Deliverer, clk *clock, leader bool, maxAttempts int) *Dispatcher {
	return NewDispatcher(DispatcherOptions{
		State: f.state, Cluster: f.node, Keyring: f.kr, Sender: s,
		IsLeader:    func() bool { return leader },
		MaxAttempts: maxAttempts,
		Logger:      slog.New(slog.DiscardHandler),
		Now:         clk.now,
		Jitter:      func() float64 { return 0.5 }, // factor 1.0 keeps the backoff exact
	})
}

// clock is an injected leader clock; tests set it between dispatcher calls.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// fakeSender records the payloads it gets and returns a configurable error.
type fakeSender struct {
	mu    sync.Mutex
	calls []cluster.AlertPayload
	err   error
}

func (s *fakeSender) Send(_ context.Context, _ Target, p cluster.AlertPayload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, p)
	return s.err
}

func (s *fakeSender) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *fakeSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// downAlert creates a monitor with one channel and sets it DOWN one second
// after its creation. It returns the clock and the alert ID of the DOWN event.
func downAlert(t *testing.T, f *fixture, kind string, resend int) (*clock, string) {
	t.Helper()
	start := time.Now()
	f.channel(testChannelID, kind, "https://hooks.example.com/secret-path")
	f.monitor("web", resend, testChannelID)
	since := start.UnixMilli() + 1000
	f.setState(store.StatusPending, store.StatusDown, since, "11111111-1111-1111-1111-111111111111")
	return &clock{t: start.Add(time.Minute)}, fmt.Sprintf("%s:%d:DOWN", testMonitorID, since)
}

func TestDeliverDueDelivered(t *testing.T) {
	f := newFixture(t)
	clk, key := downAlert(t, f, store.ChannelWebhook, 0)
	s := &fakeSender{}

	if err := f.dispatcher(s, clk, true, 10).DeliverDue(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 1 || s.calls[0].Event != store.EventDown || s.calls[0].AlertID != key {
		t.Fatalf("calls = %+v", s.calls)
	}
	a, err := f.state.GetAlert(f.ctx, key, testChannelID)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != store.AlertDelivered || a.Attempts != 1 || a.LastError != "" {
		t.Errorf("alert = %+v, want delivered after one attempt", a)
	}
	ch, err := f.state.GetChannel(f.ctx, cluster.DefaultTenantID, testChannelID)
	if err != nil || ch.State != store.ChannelOK {
		t.Errorf("channel = %+v, %v; want state ok", ch, err)
	}
}

func TestDeliverDueTemporaryFailure(t *testing.T) {
	f := newFixture(t)
	clk, key := downAlert(t, f, store.ChannelWebhook, 0)
	s := &fakeSender{err: &DeliveryError{Msg: "webhook answered 500 Internal Server Error"}}

	if err := f.dispatcher(s, clk, true, 10).DeliverDue(f.ctx); err != nil {
		t.Fatal(err)
	}
	a, err := f.state.GetAlert(f.ctx, key, testChannelID)
	if err != nil {
		t.Fatal(err)
	}
	want := clk.now().Add(30 * time.Second).UnixMilli()
	if a.State != store.AlertPending || a.Attempts != 1 || a.NextAttemptMs != want {
		t.Errorf("alert = %+v, want pending after 1 attempt, next attempt at %d", a, want)
	}
	if a.LastError != "webhook answered 500 Internal Server Error" {
		t.Errorf("LastError = %q", a.LastError)
	}
}

func TestDeliverDueGivesUpAfterMaxAttempts(t *testing.T) {
	f := newFixture(t)
	clk, key := downAlert(t, f, store.ChannelWebhook, 0)
	s := &fakeSender{err: &DeliveryError{Msg: "webhook answered 503 Service Unavailable"}}
	d := f.dispatcher(s, clk, true, 2)

	if err := d.DeliverDue(f.ctx); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour) // the retry is due after the backoff
	if err := d.DeliverDue(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 2 {
		t.Fatalf("calls = %d, want 2", s.count())
	}
	a, err := f.state.GetAlert(f.ctx, key, testChannelID)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != store.AlertFailed || a.Attempts != 2 {
		t.Errorf("alert = %+v, want failed after 2 attempts", a)
	}
	ch, err := f.state.GetChannel(f.ctx, cluster.DefaultTenantID, testChannelID)
	if err != nil || ch.State != store.ChannelFailing || ch.LastError != "webhook answered 503 Service Unavailable" {
		t.Errorf("channel = %+v, %v; want state failing", ch, err)
	}
}

func TestDeliverDuePermanentFailureDisablesChannel(t *testing.T) {
	f := newFixture(t)
	clk, key := downAlert(t, f, store.ChannelWebhook, 0)
	s := &fakeSender{err: &DeliveryError{Permanent: true, Msg: "webhook answered 401 Unauthorized"}}
	d := f.dispatcher(s, clk, true, 10)

	if err := d.DeliverDue(f.ctx); err != nil {
		t.Fatal(err)
	}
	a, err := f.state.GetAlert(f.ctx, key, testChannelID)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != store.AlertFailed {
		t.Errorf("alert state = %s, want failed", a.State)
	}
	ch, err := f.state.GetChannel(f.ctx, cluster.DefaultTenantID, testChannelID)
	if err != nil || ch.State != store.ChannelDisabled {
		t.Fatalf("channel = %+v, %v; want state disabled", ch, err)
	}

	// The monitor recovers and fails again; the disabled channel gets nothing.
	s.setErr(nil)
	recovered := time.Now().UnixMilli() + 5000
	f.setState(store.StatusDown, store.StatusUp, recovered, "")
	f.setState(store.StatusUp, store.StatusDown, recovered+1000, "22222222-2222-2222-2222-222222222222")
	if due := f.pending(clk.now().Add(time.Hour).UnixMilli()); len(due) != 0 {
		t.Errorf("disabled channel got entries: %+v", due)
	}
}

func TestRunDeliversOnlyAsLeader(t *testing.T) {
	for _, leader := range []bool{false, true} {
		t.Run(fmt.Sprintf("leader=%v", leader), func(t *testing.T) {
			f := newFixture(t)
			clk, key := downAlert(t, f, store.ChannelWebhook, 0)
			s := &fakeSender{}
			d := NewDispatcher(DispatcherOptions{
				State: f.state, Cluster: f.node, Keyring: f.kr, Sender: s,
				IsLeader: func() bool { return leader }, Logger: slog.New(slog.DiscardHandler),
				Now: clk.now, Jitter: func() float64 { return 0.5 },
				Interval: 5 * time.Millisecond, ReminderInterval: 5 * time.Millisecond,
			})
			ctx, cancel := context.WithTimeout(f.ctx, 300*time.Millisecond)
			defer cancel()
			d.Run(ctx) // returns when ctx ends

			a, err := f.state.GetAlert(f.ctx, key, testChannelID)
			if err != nil {
				t.Fatal(err)
			}
			if leader && a.State != store.AlertDelivered {
				t.Errorf("leader did not deliver: %+v", a)
			}
			if !leader && (s.count() != 0 || a.State != store.AlertPending) {
				t.Errorf("follower delivered: calls %d, alert %+v", s.count(), a)
			}
		})
	}
}

func TestEnqueueRemindersOncePerPeriod(t *testing.T) {
	f := newFixture(t)
	f.channel(testChannelID, store.ChannelWebhook, "https://hooks.example.com/x")
	f.monitor("web", 1, testChannelID)
	since := time.Now().UnixMilli() + 1000
	f.setState(store.StatusPending, store.StatusDown, since, "11111111-1111-1111-1111-111111111111")

	clk := &clock{t: time.UnixMilli(since + 150_000)} // 2.5 resend periods later
	d := f.dispatcher(&fakeSender{}, clk, true, 10)
	for range 2 {
		if err := d.EnqueueReminders(f.ctx); err != nil {
			t.Fatal(err)
		}
	}

	var reminders []string
	for _, a := range f.pending(clk.now().UnixMilli()) {
		if strings.Contains(a.AlertKey, ":reminder:") {
			reminders = append(reminders, a.AlertKey)
		}
	}
	want := fmt.Sprintf("%s:%d:DOWN:reminder:2", testMonitorID, since)
	if len(reminders) != 1 || reminders[0] != want {
		t.Errorf("reminders = %v, want exactly [%s]", reminders, want)
	}
}

// TestWebhookEndToEnd sends through the real Sender and checks that the body
// is valid JSON with the exact monitor name (FA-ALR-07).
func TestWebhookEndToEnd(t *testing.T) {
	const name = "api \"prod\" \\ eu\nnode </script>"
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	f := newFixture(t)
	f.channel(testChannelID, store.ChannelWebhook, srv.URL+"/hook")
	f.monitor(name, 0, testChannelID)
	since := time.Now().UnixMilli() + 1000
	f.setState(store.StatusPending, store.StatusDown, since, "11111111-1111-1111-1111-111111111111")

	clk := &clock{t: time.Now().Add(time.Minute)}
	sender := NewSender(&checker.Dialer{}, slog.New(slog.DiscardHandler))
	if err := f.dispatcher(sender, clk, true, 10).DeliverDue(f.ctx); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("webhook received %d bodies, want 1", len(bodies))
	}
	var got struct {
		Monitor struct {
			Name string `json:"name"`
		} `json:"monitor"`
	}
	if err := json.Unmarshal(bodies[0], &got); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, bodies[0])
	}
	if got.Monitor.Name != name {
		t.Errorf("monitor name = %q, want %q", got.Monitor.Name, name)
	}
}

// TestDeletedChannelDropsPendingAlerts checks that deleting a channel removes
// its outbox entries (schema cascade), so nothing is sent to it afterwards.
func TestDeletedChannelDropsPendingAlerts(t *testing.T) {
	f := newFixture(t)
	clk, key := downAlert(t, f, store.ChannelWebhook, 0)
	cmd := cluster.NewDeleteChannel("", cluster.DefaultTenantID, testChannelID)
	if _, err := f.node.Apply(f.ctx, cmd); err != nil {
		t.Fatal(err)
	}
	s := &fakeSender{}
	if err := f.dispatcher(s, clk, true, 10).DeliverDue(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 0 {
		t.Errorf("sent to a deleted channel")
	}
	if _, err := f.state.GetAlert(f.ctx, key, testChannelID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetAlert after channel delete: err = %v, want ErrNotFound", err)
	}
}

// TestDisabledChannelFailsPendingEntries checks that an entry still pending
// for a channel disabled by a permanent failure is not sent, and that it fails
// with the channel's error (FA-ALR-05).
func TestDisabledChannelFailsPendingEntries(t *testing.T) {
	f := newFixture(t)
	clk, first := downAlert(t, f, store.ChannelWebhook, 1)
	// One minute later the reminder is due; the first alert is still pending.
	clk.t = clk.t.Add(time.Minute)
	s := &fakeSender{}
	d := f.dispatcher(s, clk, true, 10)
	if err := d.EnqueueReminders(f.ctx); err != nil {
		t.Fatal(err)
	}
	reminder := first + ":reminder:1"
	if a, err := f.state.GetAlert(f.ctx, reminder, testChannelID); err != nil || a.State != store.AlertPending {
		t.Fatalf("reminder = %+v, %v; want pending", a, err)
	}

	// The first alert fails permanently, which disables the channel.
	a, err := f.state.GetAlert(f.ctx, first, testChannelID)
	if err != nil {
		t.Fatal(err)
	}
	ack := cluster.NewAckAlertDelivery(a, 1, cluster.DeliveryOutcome{Permanent: true, Error: "webhook answered 401 Unauthorized"})
	if _, err := f.node.Apply(f.ctx, ack); err != nil {
		t.Fatal(err)
	}
	ch, err := f.state.GetChannel(f.ctx, cluster.DefaultTenantID, testChannelID)
	if err != nil || ch.State != store.ChannelDisabled {
		t.Fatalf("channel = %+v, %v; want state disabled", ch, err)
	}

	if err := d.DeliverDue(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 0 {
		t.Errorf("sent through a disabled channel: %+v", s.calls)
	}
	got, err := f.state.GetAlert(f.ctx, reminder, testChannelID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.AlertFailed || got.LastError != "webhook answered 401 Unauthorized" {
		t.Errorf("reminder = %+v, want failed with the channel's error", got)
	}
	ch, err = f.state.GetChannel(f.ctx, cluster.DefaultTenantID, testChannelID)
	if err != nil || ch.State != store.ChannelDisabled || ch.LastError != "webhook answered 401 Unauthorized" {
		t.Errorf("channel = %+v, %v; want still disabled with its error", ch, err)
	}
}

// stepDownSender loses leadership during its first send, as a node does when
// Raft hands the leadership over while a batch is running.
type stepDownSender struct {
	leader *atomic.Bool
	calls  atomic.Int32
}

func (s *stepDownSender) Send(context.Context, Target, cluster.AlertPayload) error {
	s.calls.Add(1)
	s.leader.Store(false)
	return nil
}

// TestDeliverDueStopsAfterLeadershipLoss checks that a node which lost
// leadership during a batch sends nothing more; the entries stay pending for
// the new leader (FA-ALR-03).
func TestDeliverDueStopsAfterLeadershipLoss(t *testing.T) {
	f := newFixture(t)
	f.channel(testChannelID, store.ChannelWebhook, "https://hooks.example.com/a")
	f.channel("ch2", store.ChannelWebhook, "https://hooks.example.com/b")
	f.monitor("web", 0, testChannelID, "ch2")
	since := time.Now().UnixMilli() + 1000
	f.setState(store.StatusPending, store.StatusDown, since, "11111111-1111-1111-1111-111111111111")
	clk := &clock{t: time.Now().Add(time.Minute)}

	var leader atomic.Bool
	leader.Store(true)
	s := &stepDownSender{leader: &leader}
	d := NewDispatcher(DispatcherOptions{
		State: f.state, Cluster: f.node, Keyring: f.kr, Sender: s,
		IsLeader: leader.Load, Logger: slog.New(slog.DiscardHandler),
		Now: clk.now, Jitter: func() float64 { return 0.5 },
		Concurrency: 1, // one entry at a time, so the step-down happens between sends
	})
	if err := d.DeliverDue(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := s.calls.Load(); got != 1 {
		t.Fatalf("sends = %d, want 1 after losing leadership", got)
	}
	var delivered, pending int
	for _, ch := range []string{testChannelID, "ch2"} {
		a, err := f.state.GetAlert(f.ctx, fmt.Sprintf("%s:%d:DOWN", testMonitorID, since), ch)
		if err != nil {
			t.Fatal(err)
		}
		switch a.State {
		case store.AlertDelivered:
			delivered++
		case store.AlertPending:
			pending++
		}
	}
	if delivered != 1 || pending != 1 {
		t.Errorf("delivered = %d, pending = %d; want one of each", delivered, pending)
	}
}
