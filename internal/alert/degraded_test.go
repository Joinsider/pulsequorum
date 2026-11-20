// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package alert

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/store"
)

// doneLog records the outcomes reported to Done per alert ID.
type doneLog struct {
	mu    sync.Mutex
	calls map[string][]string
}

func newDoneLog() *doneLog { return &doneLog{calls: make(map[string][]string)} }

// fn returns the Done callback of the alert id.
func (l *doneLog) fn(id string) func(string, error) {
	return func(outcome string, err error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if err != nil {
			outcome += ": " + err.Error()
		}
		l.calls[id] = append(l.calls[id], outcome)
	}
}

// outcome returns the Done calls of id.
func (l *doneLog) outcome(id string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls[id]...)
}

// localDel returns a delivery of alert id on channel ch of the test tenant.
func localDel(id, ch string, kind string, l *doneLog) LocalDelivery {
	return LocalDelivery{
		TenantID: cluster.DefaultTenantID, ChannelID: ch,
		Target:  Target{Kind: kind, URL: "https://hooks.example.com/secret"},
		Payload: cluster.AlertPayload{AlertID: id, MonitorID: id, Event: store.EventDown, Status: "DOWN"},
		Done:    l.fn(id),
	}
}

// localDispatcher returns a dispatcher with a fixed jitter, so backoffs are exact.
func localDispatcher(s Deliverer, clk *clock, window time.Duration, maxAttempts int) *LocalDispatcher {
	return NewLocalDispatcher(LocalDispatcherOptions{
		Sender: s, BundleWindow: window, MaxAttempts: maxAttempts,
		Now: clk.now, Jitter: func() float64 { return 0.5 },
	})
}

// TestLocalRateLimit checks the token bucket: a burst of five, then one per second.
func TestLocalRateLimit(t *testing.T) {
	l := newDoneLog()
	clk := newClock()
	s := &recordingSender{}
	d := localDispatcher(s, clk, 0, 5)
	for i := range 7 {
		d.Enqueue(localDel(fmt.Sprintf("a%d", i), "ch1", store.ChannelWebhook, l))
	}

	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 5, 0)
	d.deliverDue(context.Background(), clk.now()) // same instant: no token refilled
	wantCounts(t, s, 5, 0)

	clk.t = clk.t.Add(time.Second)
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 6, 0)

	clk.t = clk.t.Add(2 * time.Second)
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 7, 0)
	for i := range 7 {
		if got := l.outcome(fmt.Sprintf("a%d", i)); len(got) != 1 || got[0] != LocalOutcomeSent {
			t.Errorf("a%d: Done calls = %v, want one sent", i, got)
		}
	}
}

// TestLocalRateLimitPerChannel checks that buckets are per channel.
func TestLocalRateLimitPerChannel(t *testing.T) {
	l := newDoneLog()
	clk := newClock()
	s := &recordingSender{}
	d := localDispatcher(s, clk, 0, 5)
	for i := range 5 {
		d.Enqueue(localDel(fmt.Sprintf("a%d", i), "ch1", store.ChannelWebhook, l))
		d.Enqueue(localDel(fmt.Sprintf("b%d", i), "ch2", store.ChannelWebhook, l))
	}
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 10, 0)
}

// localUp returns a delivery of an UP alert, like localDel does for DOWN.
func localUp(id, ch string, kind string, l *doneLog) LocalDelivery {
	del := localDel(id, ch, kind, l)
	del.Payload.Event = store.EventUp
	del.Payload.Status = "UP"
	return del
}

// TestLocalBundleWithinWindow checks that the first Shoutrrr alert goes out at
// once and the UP alerts due inside the window go out as one bundle at its end.
func TestLocalBundleWithinWindow(t *testing.T) {
	l := newDoneLog()
	clk := newClock()
	s := &recordingSender{}
	d := localDispatcher(s, clk, 30*time.Second, 5)
	d.Enqueue(localDel("m1", "ch1", store.ChannelShoutrrr, l))
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 1, 0)

	d.Enqueue(localUp("m2", "ch1", store.ChannelShoutrrr, l))
	d.Enqueue(localUp("m3", "ch1", store.ChannelShoutrrr, l))
	t0 := clk.t
	clk.t = t0.Add(5 * time.Second)
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 1, 0)
	if len(l.outcome("m2")) != 0 {
		t.Fatal("m2 finished inside the window")
	}

	clk.t = t0.Add(30 * time.Second)
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 1, 1)
	if len(s.bundles[0]) != 2 {
		t.Fatalf("bundle has %d alerts, want 2", len(s.bundles[0]))
	}
	for _, id := range []string{"m1", "m2", "m3"} {
		if got := l.outcome(id); len(got) != 1 || got[0] != LocalOutcomeSent {
			t.Errorf("%s: Done calls = %v, want one sent", id, got)
		}
	}
}

// TestLocalDownFlushesHeldUp checks that a DOWN due inside the window goes out
// at once with the UP alerts held back, as one bundle. The window end does not
// move: an UP due after the flush waits for the original end.
func TestLocalDownFlushesHeldUp(t *testing.T) {
	l := newDoneLog()
	clk := newClock()
	s := &recordingSender{}
	d := localDispatcher(s, clk, 30*time.Second, 5)
	t0 := clk.t
	d.Enqueue(localDel("m1", "ch1", store.ChannelShoutrrr, l))
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 1, 0)

	d.Enqueue(localUp("u1", "ch1", store.ChannelShoutrrr, l))
	clk.t = t0.Add(time.Second)
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 1, 0)
	if len(l.outcome("u1")) != 0 {
		t.Fatal("u1 finished inside the window")
	}

	d.Enqueue(localDel("m2", "ch1", store.ChannelShoutrrr, l))
	clk.t = t0.Add(2 * time.Second)
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 1, 1)
	if len(s.bundles[0]) != 2 {
		t.Fatalf("bundle has %d alerts, want 2", len(s.bundles[0]))
	}
	for _, id := range []string{"u1", "m2"} {
		if got := l.outcome(id); len(got) != 1 || got[0] != LocalOutcomeSent {
			t.Errorf("%s: Done calls = %v, want one sent", id, got)
		}
	}

	d.Enqueue(localUp("u2", "ch1", store.ChannelShoutrrr, l))
	clk.t = t0.Add(29 * time.Second)
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 1, 1)
	if len(l.outcome("u2")) != 0 {
		t.Fatal("u2 finished before the window end")
	}

	clk.t = t0.Add(30 * time.Second)
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 2, 1)
	if got := l.outcome("u2"); len(got) != 1 || got[0] != LocalOutcomeSent {
		t.Errorf("u2: Done calls = %v, want one sent", got)
	}
}

// TestLocalDownWithoutTokenWaits checks that a DOWN flush inside the window
// still needs a rate-limit token. Without one it waits for a later round.
func TestLocalDownWithoutTokenWaits(t *testing.T) {
	l := newDoneLog()
	clk := newClock()
	s := &recordingSender{}
	d := localDispatcher(s, clk, 30*time.Second, 5)
	t0 := clk.t
	d.Enqueue(localDel("m0", "ch1", store.ChannelShoutrrr, l))
	d.deliverDue(context.Background(), clk.now()) // opens the window, one token used
	wantCounts(t, s, 1, 0)

	for i := 1; i <= 4; i++ {
		d.Enqueue(localDel(fmt.Sprintf("m%d", i), "ch1", store.ChannelShoutrrr, l))
		d.deliverDue(context.Background(), clk.now())
	}
	wantCounts(t, s, 5, 0)

	d.Enqueue(localDel("m5", "ch1", store.ChannelShoutrrr, l))
	d.deliverDue(context.Background(), clk.now()) // no token left at this instant
	wantCounts(t, s, 5, 0)
	if len(l.outcome("m5")) != 0 {
		t.Fatal("m5 finished without a token")
	}

	clk.t = t0.Add(time.Second)
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 6, 0)
	if got := l.outcome("m5"); len(got) != 1 || got[0] != LocalOutcomeSent {
		t.Errorf("m5: Done calls = %v, want one sent", got)
	}
}

// TestLocalWebhookNeverBundled checks that webhooks get one request per alert.
func TestLocalWebhookNeverBundled(t *testing.T) {
	l := newDoneLog()
	clk := newClock()
	s := &recordingSender{}
	d := localDispatcher(s, clk, 30*time.Second, 5)
	for i := range 3 {
		d.Enqueue(localDel(fmt.Sprintf("w%d", i), "ch1", store.ChannelWebhook, l))
	}
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 3, 0)
}

// TestLocalRetryAfterBlocksChannel checks that a 429 holds back the channel
// until the Retry-After has passed, and that the held entries are not counted.
func TestLocalRetryAfterBlocksChannel(t *testing.T) {
	l := newDoneLog()
	clk := newClock()
	s := &recordingSender{err: &DeliveryError{RetryAfter: time.Minute, Msg: "webhook answered 429 Too Many Requests"}}
	d := localDispatcher(s, clk, 0, 5)
	for i := range 3 {
		d.Enqueue(localDel(fmt.Sprintf("a%d", i), "ch1", store.ChannelWebhook, l))
	}

	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 1, 0)
	clk.t = clk.t.Add(30 * time.Second)
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 1, 0)

	s.setErr(nil)
	clk.t = clk.t.Add(31 * time.Second) // one minute after the 429
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 4, 0) // the retried a0 and the two held entries
	for _, id := range []string{"a0", "a1", "a2"} {
		if got := l.outcome(id); len(got) != 1 || got[0] != LocalOutcomeSent {
			t.Errorf("%s: Done calls = %v, want one sent", id, got)
		}
	}
}

// TestLocalRetriesUntilMaxAttempts checks the backoff of 30 s, 60 s and so on,
// and that the last attempt fails the delivery exactly once.
func TestLocalRetriesUntilMaxAttempts(t *testing.T) {
	l := newDoneLog()
	clk := newClock()
	s := &recordingSender{err: &DeliveryError{Msg: "webhook answered 503 Service Unavailable"}}
	d := localDispatcher(s, clk, 0, 3)
	d.Enqueue(localDel("a0", "ch1", store.ChannelWebhook, l))

	d.deliverDue(context.Background(), clk.now()) // attempt 1, next after 30 s
	clk.t = clk.t.Add(29 * time.Second)
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 1, 0)

	clk.t = clk.t.Add(time.Second)
	d.deliverDue(context.Background(), clk.now()) // attempt 2, next after 60 s
	wantCounts(t, s, 2, 0)
	clk.t = clk.t.Add(59 * time.Second)
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 2, 0)

	clk.t = clk.t.Add(time.Second)
	d.deliverDue(context.Background(), clk.now()) // attempt 3: the last one
	clk.t = clk.t.Add(24 * time.Hour)
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 3, 0)

	got := l.outcome("a0")
	if len(got) != 1 || got[0] != LocalOutcomeFailed+": webhook answered 503 Service Unavailable" {
		t.Errorf("Done calls = %v, want one failed", got)
	}
}

// TestLocalPermanentFailureNotRetried checks that a permanent failure ends at once.
func TestLocalPermanentFailureNotRetried(t *testing.T) {
	l := newDoneLog()
	clk := newClock()
	s := &recordingSender{err: &DeliveryError{Permanent: true, Msg: "webhook answered 401 Unauthorized"}}
	d := localDispatcher(s, clk, 0, 5)
	d.Enqueue(localDel("a0", "ch1", store.ChannelWebhook, l))
	d.deliverDue(context.Background(), clk.now())
	clk.t = clk.t.Add(time.Hour)
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 1, 0)
	if got := l.outcome("a0"); len(got) != 1 || got[0] != LocalOutcomeFailed+": webhook answered 401 Unauthorized" {
		t.Errorf("Done calls = %v, want one failed", got)
	}
}

// TestLocalRunDeliversAndFailsPendingOnStop checks Run: five alerts go out at
// once, the two waiting for a token fail when ctx ends, and each Done is called
// exactly once.
func TestLocalRunDeliversAndFailsPendingOnStop(t *testing.T) {
	l := newDoneLog()
	s := &recordingSender{}
	d := NewLocalDispatcher(LocalDispatcherOptions{Sender: s, Interval: 5 * time.Millisecond})
	for i := range 7 {
		d.Enqueue(localDel(fmt.Sprintf("a%d", i), "ch1", store.ChannelWebhook, l))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	d.Run(ctx) // returns when ctx ends

	wantCounts(t, s, 5, 0)
	sent, failed := 0, 0
	for i := range 7 {
		got := l.outcome(fmt.Sprintf("a%d", i))
		if len(got) != 1 {
			t.Errorf("a%d: Done calls = %v, want exactly one", i, got)
			continue
		}
		switch got[0] {
		case LocalOutcomeSent:
			sent++
		default:
			failed++
			if want := LocalOutcomeFailed + ": " + context.DeadlineExceeded.Error(); got[0] != want {
				t.Errorf("a%d: Done = %q, want %q", i, got[0], want)
			}
		}
	}
	if sent != 5 || failed != 2 {
		t.Errorf("sent = %d, failed = %d; want 5 and 2", sent, failed)
	}
}

// TestLocalEnqueueAfterStopFails checks that a delivery arriving after Run has
// ended fails at once.
func TestLocalEnqueueAfterStopFails(t *testing.T) {
	d := NewLocalDispatcher(LocalDispatcherOptions{Sender: &recordingSender{}, Interval: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.Run(ctx)

	l := newDoneLog()
	d.Enqueue(localDel("late", "ch1", store.ChannelWebhook, l))
	if got := l.outcome("late"); len(got) != 1 || got[0] != LocalOutcomeFailed+": local dispatcher stopped" {
		t.Errorf("Done calls = %v, want one failed", got)
	}
}

// TestLocalWebhookUnaffectedByDownRule checks that webhooks keep one request per
// alert, DOWN and UP alike, with a bundling window set.
func TestLocalWebhookUnaffectedByDownRule(t *testing.T) {
	l := newDoneLog()
	clk := newClock()
	s := &recordingSender{}
	d := localDispatcher(s, clk, 30*time.Second, 5)
	d.Enqueue(localDel("m1", "ch1", store.ChannelWebhook, l))
	d.Enqueue(localUp("u1", "ch1", store.ChannelWebhook, l))
	d.Enqueue(localDel("m2", "ch1", store.ChannelWebhook, l))
	d.deliverDue(context.Background(), clk.now())
	wantCounts(t, s, 3, 0)
}
