// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package alert

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/store"
)

// Outcomes reported to LocalDelivery.Done (ADR 0024, section 3). Suppressed
// alerts never reach the dispatcher.
const (
	LocalOutcomeSent   = "sent"
	LocalOutcomeFailed = "failed"
)

// localMaxAttempts is the default number of attempts of a local delivery.
const localMaxAttempts = 5

// errLocalStopped is reported for deliveries that arrive after Run has ended.
var errLocalStopped = errors.New("local dispatcher stopped")

// LocalDelivery is one alert of the degraded mode. It lives in memory only;
// the caller records the outcome in its own log.
type LocalDelivery struct {
	// TenantID and ChannelID identify the channel for rate limit and bundling.
	TenantID  string
	ChannelID string
	Target    Target
	Payload   cluster.AlertPayload
	// Done is called exactly once with LocalOutcomeSent or LocalOutcomeFailed.
	// It may be nil.
	Done func(outcome string, err error)
}

// LocalDispatcherOptions configures a LocalDispatcher. Zero values select the
// defaults, except for Sender, which is required.
type LocalDispatcherOptions struct {
	// Sender delivers the alerts. If it also implements BundleDeliverer,
	// Shoutrrr alerts can be bundled.
	Sender Deliverer
	// BundleWindow bundles the Shoutrrr alerts of one channel as in the
	// Dispatcher (ADR 0023). Zero disables bundling.
	BundleWindow time.Duration
	// MaxAttempts is the number of attempts before a delivery fails (default 5).
	MaxAttempts int
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Jitter returns a value in [0,1) that spreads retries (default math/rand/v2).
	Jitter func() float64
	// Interval is the period in which due deliveries are checked (default 500 ms).
	Interval time.Duration
	Logger   *slog.Logger
}

// LocalDispatcher delivers the alerts of the degraded mode (ADR 0024). It keeps
// everything in memory and shares the rate limit, Retry-After block, bundling
// and backoff of the Dispatcher (ADR 0014, 0023). Deliveries are not
// at-least-once: a delivery pending at shutdown fails.
type LocalDispatcher struct {
	opts    LocalDispatcherOptions
	bundler BundleDeliverer // nil if the sender cannot bundle
	wake    chan struct{}

	// mu guards pending and stopped.
	mu      sync.Mutex
	pending []*localEntry
	stopped bool

	// channels holds the rate and bundle state per channel. Only deliverDue
	// reads and writes it, so it needs no lock.
	channels map[channelKey]*channelState
}

// localEntry is one pending delivery and its attempts.
type localEntry struct {
	del      LocalDelivery
	attempts int
	// nextAttempt is the earliest time of the next attempt (zero: now).
	nextAttempt time.Time
}

// NewLocalDispatcher creates a dispatcher with defaults applied to opts.
func NewLocalDispatcher(opts LocalDispatcherOptions) *LocalDispatcher {
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = localMaxAttempts
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Jitter == nil {
		// Jitter only spreads retries; it is not security-relevant.
		opts.Jitter = rand.Float64
	}
	if opts.Interval <= 0 {
		opts.Interval = 500 * time.Millisecond
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	bundler, _ := opts.Sender.(BundleDeliverer)
	return &LocalDispatcher{
		opts:     opts,
		bundler:  bundler,
		wake:     make(chan struct{}, 1),
		channels: make(map[channelKey]*channelState),
	}
}

// Enqueue adds a delivery. It returns at once; the outcome arrives through
// Done. A delivery enqueued after Run has ended fails at once.
func (d *LocalDispatcher) Enqueue(del LocalDelivery) {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		finishLocal(del, LocalOutcomeFailed, errLocalStopped)
		return
	}
	d.pending = append(d.pending, &localEntry{del: del})
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run delivers the pending alerts until ctx ends. On the end, every pending
// delivery fails with ctx.Err(). Run is meant to be called once.
func (d *LocalDispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(d.opts.Interval)
	defer ticker.Stop()
	for {
		d.deliverDue(ctx, d.opts.Now())
		select {
		case <-ctx.Done():
			d.stop(ctx.Err())
			return
		case <-ticker.C:
		case <-d.wake:
		}
	}
}

// stop ends the dispatcher and fails every pending delivery with err.
func (d *LocalDispatcher) stop(err error) {
	d.mu.Lock()
	d.stopped = true
	dropped := d.pending
	d.pending = nil
	d.mu.Unlock()
	for _, e := range dropped {
		finishLocal(e.del, LocalOutcomeFailed, err)
	}
}

// deliverDue sends the due deliveries that the rate limit, the Retry-After
// block and the bundling window allow at now. Channels run in parallel, the
// messages of one channel one after the other. It returns when all are done.
func (d *LocalDispatcher) deliverDue(ctx context.Context, now time.Time) {
	d.mu.Lock()
	var keys []channelKey
	due := make(map[channelKey][]*localEntry)
	for _, e := range d.pending {
		if e.nextAttempt.After(now) {
			continue
		}
		k := channelKey{tenant: e.del.TenantID, channel: e.del.ChannelID}
		if _, ok := due[k]; !ok {
			keys = append(keys, k)
		}
		due[k] = append(due[k], e)
	}
	d.mu.Unlock()

	// The channel state is read here, before the goroutines start; each
	// goroutine then owns its state exclusively.
	var wg sync.WaitGroup
	for _, k := range keys {
		st := d.channels[k]
		if st == nil {
			st = &channelState{tokens: rateBurst, last: now}
			d.channels[k] = st
		}
		wg.Add(1)
		go func(st *channelState, entries []*localEntry) {
			defer wg.Done()
			d.runChannel(ctx, now, st, entries)
		}(st, due[k])
	}
	wg.Wait()
}

// runChannel plans and sends the due entries of one channel at now.
func (d *LocalDispatcher) runChannel(ctx context.Context, now time.Time, st *channelState, entries []*localEntry) {
	if now.Before(st.blockedUntil) || ctx.Err() != nil {
		return
	}
	if d.opts.BundleWindow > 0 && d.bundler != nil && entries[0].del.Target.Kind == store.ChannelShoutrrr {
		// As in the Dispatcher: the first alert goes out at once, alerts due
		// inside the window wait, and at its end all due alerts go out as one.
		if now.Before(st.windowEnd) || !st.take(now) {
			return
		}
		st.windowEnd = now.Add(d.opts.BundleWindow)
		d.send(ctx, st, entries)
		return
	}
	for _, e := range entries {
		if !st.take(now) || now.Before(st.blockedUntil) {
			return // the rest waits for a later round
		}
		d.send(ctx, st, []*localEntry{e})
	}
}

// send makes one message of entries and records the outcome of each entry: a
// delivered or finally failed entry leaves the pending list, a retried one
// stays with its next attempt.
func (d *LocalDispatcher) send(ctx context.Context, st *channelState, entries []*localEntry) {
	var err error
	if len(entries) == 1 {
		err = d.opts.Sender.Send(ctx, entries[0].del.Target, entries[0].del.Payload)
	} else {
		ps := make([]cluster.AlertPayload, len(entries))
		for i, e := range entries {
			ps[i] = e.del.Payload
		}
		err = d.bundler.SendBundle(ctx, entries[0].del.Target, ps)
	}
	if err != nil && ctx.Err() != nil {
		// Cut short by shutdown: Run fails the entries that are still pending.
		for _, e := range entries {
			d.finish(e, LocalOutcomeFailed, ctx.Err())
		}
		return
	}

	res := classify(err)
	if res.retryAfter > 0 {
		if until := d.opts.Now().Add(res.retryAfter); until.After(st.blockedUntil) {
			st.blockedUntil = until
		}
	}
	for _, e := range entries {
		e.attempts++
		switch {
		case res.delivered:
			d.finish(e, LocalOutcomeSent, nil)
		case res.permanent || e.attempts >= d.opts.MaxAttempts:
			d.logFailure(e, res, false)
			d.finish(e, LocalOutcomeFailed, errors.New(res.msg))
		default:
			wait := max(backoffDelay(e.attempts, d.opts.Jitter()), res.retryAfter)
			e.nextAttempt = d.opts.Now().Add(wait)
			d.logFailure(e, res, true)
		}
	}
}

// finish removes a delivery from the pending list and reports its final
// outcome. Retries never come here: they keep the entry pending.
func (d *LocalDispatcher) finish(e *localEntry, outcome string, err error) {
	d.remove(e)
	finishLocal(e.del, outcome, err)
}

func (d *LocalDispatcher) remove(e *localEntry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, p := range d.pending {
		if p == e {
			d.pending = append(d.pending[:i], d.pending[i+1:]...)
			return
		}
	}
}

func (d *LocalDispatcher) logFailure(e *localEntry, res result, retry bool) {
	d.opts.Logger.Warn("degraded alert delivery failed",
		"tenant", e.del.TenantID, "channel", e.del.ChannelID, "monitor", e.del.Payload.MonitorID,
		"attempt", e.attempts, "error", res.msg, "permanent", res.permanent, "retry", retry)
}

// finishLocal calls Done once, if set.
func finishLocal(del LocalDelivery, outcome string, err error) {
	if del.Done != nil {
		del.Done(outcome, err)
	}
}
