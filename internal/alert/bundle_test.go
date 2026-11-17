// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package alert

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/checker"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/store"
)

// recordingSender records single sends and bundles. It implements
// BundleDeliverer, so the dispatcher may bundle through it.
type recordingSender struct {
	mu      sync.Mutex
	singles []cluster.AlertPayload
	bundles [][]cluster.AlertPayload
	err     error
}

func (s *recordingSender) Send(_ context.Context, _ Target, p cluster.AlertPayload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.singles = append(s.singles, p)
	return s.err
}

func (s *recordingSender) SendBundle(_ context.Context, _ Target, ps []cluster.AlertPayload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bundles = append(s.bundles, append([]cluster.AlertPayload(nil), ps...))
	return s.err
}

func (s *recordingSender) counts() (singles, bundles int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.singles), len(s.bundles)
}

func (s *recordingSender) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// newClock returns a leader clock that is past the creation of the test alerts.
func newClock() *clock {
	return &clock{t: time.Now().Add(time.Minute)}
}

func leaderOn() *atomic.Bool {
	var b atomic.Bool
	b.Store(true)
	return &b
}

// dispatcherWith creates a dispatcher whose leadership is controlled by leader.
func (f *fixture) dispatcherWith(s Deliverer, clk *clock, leader *atomic.Bool, window time.Duration) *Dispatcher {
	return NewDispatcher(DispatcherOptions{
		State: f.state, Cluster: f.node, Keyring: f.kr, Sender: s,
		IsLeader: leader.Load, MaxAttempts: 10, Logger: slog.New(slog.DiscardHandler),
		Now: clk.now, Jitter: func() float64 { return 0.5 }, BundleWindow: window,
	})
}

// monitorDown creates monitor id on the test channel and sets it DOWN. It
// returns the since timestamp of the DOWN period.
func (f *fixture) monitorDown(id string, n int) int64 {
	f.t.Helper()
	doc := cluster.MonitorDocument{Name: id, Type: "http", IntervalSeconds: 60, TimeoutSeconds: 10,
		RecoveryThreshold: 1, Assignment: store.Assignment{Mode: "all"},
		ConsensusPolicy: "majority", NotificationChannelIDs: []string{testChannelID}}
	cmd, err := cluster.NewPutMonitor("", cluster.DefaultTenantID, id, doc, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.node.Apply(f.ctx, cmd); err != nil {
		f.t.Fatal(err)
	}
	since := time.Now().UnixMilli() + 1000
	c := cluster.StateChange{TenantID: cluster.DefaultTenantID, MonitorID: id,
		From: store.StatusPending, To: store.StatusDown, SinceMs: since,
		Incident: &store.Incident{ID: fmt.Sprintf("%08d-1111-1111-1111-111111111111", n), Cause: "connection refused",
			ErrorClass: string(checker.ClassConnect)}}
	if _, err := f.node.Apply(f.ctx, cluster.NewSetMonitorState(c)); err != nil {
		f.t.Fatal(err)
	}
	return since
}

// entry returns the outbox entry of the DOWN alert of monitorID on the test channel.
func (f *fixture) entry(monitorID string, since int64) store.AlertDelivery {
	f.t.Helper()
	a, err := f.state.GetAlert(f.ctx, fmt.Sprintf("%s:%d:DOWN", monitorID, since), testChannelID)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

// deliver runs one dispatch round and fails the test on error.
func (f *fixture) deliver(d *Dispatcher) {
	f.t.Helper()
	if err := d.DeliverDue(f.ctx); err != nil {
		f.t.Fatal(err)
	}
}

// wantDelivered checks that an entry was delivered after exactly attempts attempts.
func (f *fixture) wantDelivered(monitorID string, since int64, attempts int) {
	f.t.Helper()
	a := f.entry(monitorID, since)
	if a.State != store.AlertDelivered || a.Attempts != attempts {
		f.t.Errorf("%s: alert = %+v, want delivered after %d attempts", monitorID, a, attempts)
	}
}

// wantUntouched checks that an entry was neither sent nor counted as an attempt.
func (f *fixture) wantUntouched(monitorID string, since int64) {
	f.t.Helper()
	a := f.entry(monitorID, since)
	if a.State != store.AlertPending || a.Attempts != 0 {
		f.t.Errorf("%s: alert = %+v, want pending without attempts", monitorID, a)
	}
}

func wantCounts(t *testing.T, s *recordingSender, singles, bundles int) {
	t.Helper()
	if gs, gb := s.counts(); gs != singles || gb != bundles {
		t.Fatalf("sends: singles = %d, bundles = %d; want %d and %d", gs, gb, singles, bundles)
	}
}

// TestBucketBurstThenOnePerSecond checks the rate limit: a burst of five, then
// one message per second. Entries without a token wait and count no attempt.
func TestBucketBurstThenOnePerSecond(t *testing.T) {
	f := newFixture(t)
	f.channel(testChannelID, store.ChannelWebhook, "https://hooks.example.com/x")
	since := make([]int64, 8)
	for i := range since {
		since[i] = f.monitorDown(fmt.Sprintf("m%d", i), i)
	}
	clk := newClock()
	s := &recordingSender{}
	d := f.dispatcherWith(s, clk, leaderOn(), 0)

	f.deliver(d)
	wantCounts(t, s, 5, 0)
	for i := 5; i < 8; i++ {
		f.wantUntouched(fmt.Sprintf("m%d", i), since[i])
	}

	f.deliver(d) // same instant: no token refilled
	wantCounts(t, s, 5, 0)

	clk.t = clk.t.Add(time.Second)
	f.deliver(d)
	wantCounts(t, s, 6, 0)

	clk.t = clk.t.Add(2 * time.Second)
	f.deliver(d)
	wantCounts(t, s, 8, 0)
	for i := range since {
		f.wantDelivered(fmt.Sprintf("m%d", i), since[i], 1)
	}
}

// TestBucketFullAfterLeadershipChange checks that a new leader starts with
// full buckets.
func TestBucketFullAfterLeadershipChange(t *testing.T) {
	f := newFixture(t)
	f.channel(testChannelID, store.ChannelWebhook, "https://hooks.example.com/x")
	for i := range 7 {
		f.monitorDown(fmt.Sprintf("m%d", i), i)
	}
	clk := newClock()
	leader := leaderOn()
	s := &recordingSender{}
	d := f.dispatcherWith(s, clk, leader, 0)

	f.deliver(d)
	wantCounts(t, s, 5, 0)

	leader.Store(false)
	f.deliver(d)
	leader.Store(true)
	f.deliver(d) // the same instant: the new leader's bucket is full again
	wantCounts(t, s, 7, 0)
}

// TestRetryAfterDelaysEntry checks that a 429 with Retry-After sets the next
// attempt to now plus the Retry-After, which is longer than the backoff.
func TestRetryAfterDelaysEntry(t *testing.T) {
	f := newFixture(t)
	f.channel(testChannelID, store.ChannelWebhook, "https://hooks.example.com/x")
	since := f.monitorDown("m1", 1)
	clk := newClock()
	s := &fakeSender{err: &DeliveryError{RetryAfter: 2 * time.Minute, Msg: "webhook answered 429 Too Many Requests"}}
	if err := f.dispatcher(s, clk, true, 10).DeliverDue(f.ctx); err != nil {
		t.Fatal(err)
	}

	a := f.entry("m1", since)
	want := clk.now().Add(2 * time.Minute).UnixMilli()
	if a.State != store.AlertPending || a.Attempts != 1 || a.NextAttemptMs != want {
		t.Errorf("alert = %+v, want pending after 1 attempt, next attempt at %d", a, want)
	}
	if a.LastError != "webhook answered 429 Too Many Requests" {
		t.Errorf("LastError = %q", a.LastError)
	}
}

// TestRetryAfterBlocksChannel checks that after a 429 the other entries of the
// channel wait without an attempt, until the Retry-After has passed.
func TestRetryAfterBlocksChannel(t *testing.T) {
	f := newFixture(t)
	f.channel(testChannelID, store.ChannelWebhook, "https://hooks.example.com/x")
	for i := range 3 {
		f.monitorDown(fmt.Sprintf("m%d", i), i)
	}
	clk := newClock()
	s := &fakeSender{err: &DeliveryError{RetryAfter: time.Minute, Msg: "webhook answered 429 Too Many Requests"}}
	d := f.dispatcher(s, clk, true, 10)

	if err := d.DeliverDue(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 1 {
		t.Fatalf("sends = %d, want 1 before the channel is blocked", s.count())
	}
	clk.t = clk.t.Add(30 * time.Second)
	if err := d.DeliverDue(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 1 {
		t.Fatalf("sends = %d, want 1 while blocked", s.count())
	}

	s.setErr(nil)
	clk.t = clk.t.Add(31 * time.Second)
	if err := d.DeliverDue(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 4 {
		t.Errorf("sends = %d, want 4 after the block ended", s.count())
	}
}

// TestBundleSendsDueAlertsAsOne checks that alerts due at the same time of a
// Shoutrrr channel go out as one message, each acknowledged on its own.
func TestBundleSendsDueAlertsAsOne(t *testing.T) {
	f := newFixture(t)
	f.channel(testChannelID, store.ChannelShoutrrr, "generic://hooks.example.com/x")
	var since [3]int64
	for i := range since {
		since[i] = f.monitorDown(fmt.Sprintf("m%d", i), i)
	}
	s := &recordingSender{}
	f.deliver(f.dispatcherWith(s, newClock(), leaderOn(), 30*time.Second))

	wantCounts(t, s, 0, 1)
	if len(s.bundles[0]) != 3 {
		t.Fatalf("bundle has %d alerts, want 3", len(s.bundles[0]))
	}
	for i := range since {
		f.wantDelivered(fmt.Sprintf("m%d", i), since[i], 1)
	}
}

// TestBundleCollectsWithinWindow checks that UP alerts becoming due inside the
// window wait until it ends and then go out as one message (ADR 0026: only
// DOWN alerts skip the wait).
func TestBundleCollectsWithinWindow(t *testing.T) {
	f := newFixture(t)
	f.channel(testChannelID, store.ChannelShoutrrr, "generic://hooks.example.com/x")
	m2 := f.monitorDown("m2", 2)
	m3 := f.monitorDown("m3", 3)
	clk := newClock()
	t0 := clk.t
	s := &recordingSender{}
	d := f.dispatcherWith(s, clk, leaderOn(), 30*time.Second)
	f.deliver(d) // both DOWN alerts go out as one bundle and open the window
	wantCounts(t, s, 0, 1)

	up2 := time.Now().UnixMilli() + 2000
	up3 := time.Now().UnixMilli() + 3000
	f.recoverMonitor("m2", up2)
	f.recoverMonitor("m3", up3)
	for _, after := range []time.Duration{5 * time.Second, 29 * time.Second} {
		clk.t = t0.Add(after)
		f.deliver(d)
		wantCounts(t, s, 0, 1)
	}
	f.wantUpPending("m2", up2)
	f.wantUpPending("m3", up3)

	clk.t = t0.Add(30 * time.Second) // the window ends 30 s after the first send
	f.deliver(d)
	wantCounts(t, s, 0, 2)
	if len(s.bundles[1]) != 2 {
		t.Fatalf("bundle has %d alerts, want 2", len(s.bundles[1]))
	}
	f.wantUpDelivered("m2", up2)
	f.wantUpDelivered("m3", up3)
	f.wantDelivered("m2", m2, 1)
	f.wantDelivered("m3", m3, 1)
}

// TestBundleWindowZeroSendsEach checks that a zero window turns bundling off.
func TestBundleWindowZeroSendsEach(t *testing.T) {
	f := newFixture(t)
	f.channel(testChannelID, store.ChannelShoutrrr, "generic://hooks.example.com/x")
	for i := range 3 {
		f.monitorDown(fmt.Sprintf("m%d", i), i)
	}
	s := &recordingSender{}
	f.deliver(f.dispatcherWith(s, newClock(), leaderOn(), 0))
	wantCounts(t, s, 3, 0)
}

// TestWebhookNeverBundled checks that webhooks get one request per alert, even
// with a bundling window.
func TestWebhookNeverBundled(t *testing.T) {
	f := newFixture(t)
	f.channel(testChannelID, store.ChannelWebhook, "https://hooks.example.com/x")
	for i := range 3 {
		f.monitorDown(fmt.Sprintf("m%d", i), i)
	}
	s := &recordingSender{}
	f.deliver(f.dispatcherWith(s, newClock(), leaderOn(), 30*time.Second))
	wantCounts(t, s, 3, 0)
}

// TestBundleFailureAcksEveryEntry checks that a failed bundle gives every
// entry the same outcome.
func TestBundleFailureAcksEveryEntry(t *testing.T) {
	f := newFixture(t)
	f.channel(testChannelID, store.ChannelShoutrrr, "generic://hooks.example.com/x")
	var since [3]int64
	for i := range since {
		since[i] = f.monitorDown(fmt.Sprintf("m%d", i), i)
	}
	clk := newClock()
	s := &recordingSender{err: &DeliveryError{Msg: "generic: 503 Service Unavailable"}}
	f.deliver(f.dispatcherWith(s, clk, leaderOn(), 30*time.Second))
	wantCounts(t, s, 0, 1)

	want := clk.now().Add(30 * time.Second).UnixMilli()
	for i := range since {
		a := f.entry(fmt.Sprintf("m%d", i), since[i])
		if a.State != store.AlertPending || a.Attempts != 1 || a.NextAttemptMs != want ||
			a.LastError != "generic: 503 Service Unavailable" {
			t.Errorf("m%d: alert = %+v, want pending after 1 attempt, next attempt at %d", i, a, want)
		}
	}
}

// TestBundleLeadershipLossDropsWindow checks that a node losing leadership in
// the middle of a window forgets it. The entries stay in the outbox, and the
// next leader sends them.
func TestBundleLeadershipLossDropsWindow(t *testing.T) {
	f := newFixture(t)
	f.channel(testChannelID, store.ChannelShoutrrr, "generic://hooks.example.com/x")
	f.monitorDown("m1", 1)
	clk := newClock()
	leader := leaderOn()
	s := &recordingSender{}
	d := f.dispatcherWith(s, clk, leader, 30*time.Second)
	f.deliver(d)
	wantCounts(t, s, 1, 0)

	s2 := f.monitorDown("m2", 2)
	s3 := f.monitorDown("m3", 3)
	clk.t = clk.t.Add(5 * time.Second)
	leader.Store(false)
	f.deliver(d)
	leader.Store(true)
	f.deliver(d) // the window is gone: the due alerts go out at once

	wantCounts(t, s, 1, 1)
	f.wantDelivered("m2", s2, 1)
	f.wantDelivered("m3", s3, 1)
}

// TestDeliverDueSkipsWithoutLeaderState checks that a follower sends nothing.
func TestDeliverDueSkipsWithoutLeaderState(t *testing.T) {
	f := newFixture(t)
	f.channel(testChannelID, store.ChannelWebhook, "https://hooks.example.com/x")
	since := f.monitorDown("m1", 1)
	s := &recordingSender{}
	f.deliver(f.dispatcherWith(s, newClock(), &atomic.Bool{}, 0))
	wantCounts(t, s, 0, 0)
	f.wantUntouched("m1", since)
}

// recoverMonitor sets the DOWN monitor id to UP at sinceMs. The change
// proposes an UP alert.
func (f *fixture) recoverMonitor(id string, sinceMs int64) {
	f.t.Helper()
	c := cluster.StateChange{TenantID: cluster.DefaultTenantID, MonitorID: id,
		From: store.StatusDown, To: store.StatusUp, SinceMs: sinceMs}
	if _, err := f.node.Apply(f.ctx, cluster.NewSetMonitorState(c)); err != nil {
		f.t.Fatal(err)
	}
}

// upEntry returns the outbox entry of the UP alert of monitorID that starts at since.
func (f *fixture) upEntry(monitorID string, since int64) store.AlertDelivery {
	f.t.Helper()
	a, err := f.state.GetAlert(f.ctx, fmt.Sprintf("%s:%d:UP", monitorID, since), testChannelID)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

// wantUpPending checks that the UP alert of monitorID is still pending and not attempted.
func (f *fixture) wantUpPending(monitorID string, since int64) {
	f.t.Helper()
	a := f.upEntry(monitorID, since)
	if a.State != store.AlertPending || a.Attempts != 0 {
		f.t.Errorf("%s UP: alert = %+v, want pending without attempts", monitorID, a)
	}
}

// wantUpDelivered checks that the UP alert of monitorID was delivered after one attempt.
func (f *fixture) wantUpDelivered(monitorID string, since int64) {
	f.t.Helper()
	a := f.upEntry(monitorID, since)
	if a.State != store.AlertDelivered || a.Attempts != 1 {
		f.t.Errorf("%s UP: alert = %+v, want delivered after 1 attempt", monitorID, a)
	}
}

// TestBundleDownFlushesHeldUp checks that a DOWN due inside the window sends at
// once, together with the UP alerts held back in the window, as one bundle. The
// window end does not move, so an UP due later in the window waits for it.
func TestBundleDownFlushesHeldUp(t *testing.T) {
	f := newFixture(t)
	f.channel(testChannelID, store.ChannelShoutrrr, "generic://hooks.example.com/x")
	m2 := f.monitorDown("m2", 2)
	clk := newClock()
	t0 := clk.t
	s := &recordingSender{}
	d := f.dispatcherWith(s, clk, leaderOn(), 30*time.Second)
	f.deliver(d) // m2 goes out at once and opens the window
	wantCounts(t, s, 1, 0)

	up2 := time.Now().UnixMilli() + 2000
	f.recoverMonitor("m2", up2)
	clk.t = t0.Add(time.Second)
	f.deliver(d) // a lone UP in the window waits
	wantCounts(t, s, 1, 0)
	f.wantUpPending("m2", up2)

	m1 := f.monitorDown("m1", 1)
	clk.t = t0.Add(2 * time.Second)
	f.deliver(d) // the DOWN flushes the held UP: one bundle of two
	wantCounts(t, s, 1, 1)
	if len(s.bundles[0]) != 2 {
		t.Fatalf("bundle has %d alerts, want 2", len(s.bundles[0]))
	}
	f.wantUpDelivered("m2", up2)
	f.wantDelivered("m1", m1, 1)
	f.wantDelivered("m2", m2, 1)

	up1 := time.Now().UnixMilli() + 2000
	f.recoverMonitor("m1", up1)
	clk.t = t0.Add(29 * time.Second)
	f.deliver(d) // the window still ends at t0+30 s
	wantCounts(t, s, 1, 1)
	f.wantUpPending("m1", up1)

	clk.t = t0.Add(30 * time.Second)
	f.deliver(d)
	wantCounts(t, s, 2, 1)
	f.wantUpDelivered("m1", up1)
}

// TestBundleUpWaitsForWindowEnd checks that a lone UP due inside the window
// waits until the window ends.
func TestBundleUpWaitsForWindowEnd(t *testing.T) {
	f := newFixture(t)
	f.channel(testChannelID, store.ChannelShoutrrr, "generic://hooks.example.com/x")
	m1 := f.monitorDown("m1", 1)
	clk := newClock()
	t0 := clk.t
	s := &recordingSender{}
	d := f.dispatcherWith(s, clk, leaderOn(), 30*time.Second)
	f.deliver(d)
	wantCounts(t, s, 1, 0)
	f.wantDelivered("m1", m1, 1)

	up1 := time.Now().UnixMilli() + 2000
	f.recoverMonitor("m1", up1)
	clk.t = t0.Add(5 * time.Second)
	f.deliver(d)
	clk.t = t0.Add(29 * time.Second)
	f.deliver(d)
	wantCounts(t, s, 1, 0)
	f.wantUpPending("m1", up1)

	clk.t = t0.Add(30 * time.Second)
	f.deliver(d)
	wantCounts(t, s, 2, 0)
	f.wantUpDelivered("m1", up1)
}

// TestBundleDownWithoutTokenWaits checks that a DOWN inside the window still
// needs a rate-limit token. Without one it waits for the next round.
func TestBundleDownWithoutTokenWaits(t *testing.T) {
	f := newFixture(t)
	f.channel(testChannelID, store.ChannelShoutrrr, "generic://hooks.example.com/x")
	m0 := f.monitorDown("m0", 0)
	clk := newClock()
	t0 := clk.t
	s := &recordingSender{}
	d := f.dispatcherWith(s, clk, leaderOn(), 30*time.Second)
	f.deliver(d) // m0 opens the window and takes one of five tokens
	wantCounts(t, s, 1, 0)
	f.wantDelivered("m0", m0, 1)

	// Four DOWN alerts in the window take the remaining four tokens.
	for i := 1; i <= 4; i++ {
		f.monitorDown(fmt.Sprintf("m%d", i), i)
		f.deliver(d)
	}
	wantCounts(t, s, 5, 0)

	m5 := f.monitorDown("m5", 5)
	f.deliver(d) // no token left at the same instant
	wantCounts(t, s, 5, 0)
	f.wantUntouched("m5", m5)

	clk.t = t0.Add(time.Second)
	f.deliver(d)
	wantCounts(t, s, 6, 0)
	f.wantDelivered("m5", m5, 1)
}

// TestWebhookUnaffectedByDownRule checks that webhooks keep one request per
// alert, UP and DOWN alike, with a bundling window set.
func TestWebhookUnaffectedByDownRule(t *testing.T) {
	f := newFixture(t)
	f.channel(testChannelID, store.ChannelWebhook, "https://hooks.example.com/x")
	m1 := f.monitorDown("m1", 1)
	clk := newClock()
	t0 := clk.t
	s := &recordingSender{}
	d := f.dispatcherWith(s, clk, leaderOn(), 30*time.Second)
	f.deliver(d)
	wantCounts(t, s, 1, 0)

	up1 := time.Now().UnixMilli() + 2000
	f.recoverMonitor("m1", up1)
	m2 := f.monitorDown("m2", 2)
	clk.t = t0.Add(time.Second)
	f.deliver(d)
	wantCounts(t, s, 3, 0)
	f.wantUpDelivered("m1", up1)
	f.wantDelivered("m1", m1, 1)
	f.wantDelivered("m2", m2, 1)
}
