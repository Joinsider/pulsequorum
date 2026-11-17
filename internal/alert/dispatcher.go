// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package alert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// Retry and timing defaults (ADR 0014, FA-ALR-03).
const (
	defaultMaxAttempts      = 10
	defaultInterval         = 2 * time.Second
	defaultReminderInterval = 30 * time.Second
	defaultConcurrency      = 4
	defaultBatchSize        = 50
	retryBase               = 30 * time.Second
	retryMax                = time.Hour
	ackTimeout              = 5 * time.Second
)

// Rate limit per channel (ADR 0023, FA-ALR-04): a token bucket that holds
// rateBurst messages and refills ratePerSecond. Every message takes one token,
// bundles included. The buckets live in memory.
const (
	ratePerSecond = 1.0
	rateBurst     = 5.0
)

// Applier proposes commands to the cluster. *cluster.Node satisfies it.
type Applier interface {
	Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error)
}

// Deliverer sends one alert to one target. *Sender satisfies it.
type Deliverer interface {
	Send(ctx context.Context, t Target, p cluster.AlertPayload) error
}

// BundleDeliverer sends several alerts as one message (ADR 0023). A dispatcher
// whose sender lacks it never bundles. *Sender satisfies it.
type BundleDeliverer interface {
	SendBundle(ctx context.Context, t Target, ps []cluster.AlertPayload) error
}

// DispatcherOptions configures the leader's outbox dispatcher. Zero values
// select the defaults, except for State, Cluster, Keyring, Sender and IsLeader,
// which are required.
type DispatcherOptions struct {
	State   store.State
	Cluster Applier
	Keyring Keyring
	Sender  Deliverer
	// IsLeader reports whether this node may propose commands; only the leader dispatches.
	IsLeader func() bool
	// MaxAttempts is the number of delivery attempts before an alert gives up (default 10).
	MaxAttempts int
	Logger      *slog.Logger
	// Now is the clock of the leader (default time.Now). Deliveries and backoff use it.
	Now func() time.Time
	// Jitter returns a value in [0,1) that spreads retries (default math/rand/v2).
	Jitter func() float64
	// Interval is the period of DeliverDue (default 2 s).
	Interval time.Duration
	// ReminderInterval is the period of EnqueueReminders (default 30 s).
	ReminderInterval time.Duration
	// Concurrency bounds the parallel deliveries (default 4).
	Concurrency int
	// BatchSize bounds the outbox entries read per DeliverDue (default 50).
	BatchSize int
	// BundleWindow bundles the alerts of one Shoutrrr channel (ADR 0023). The
	// alerts due in the window after a send wait and go out as one message at
	// its end. A DOWN alert due in the window does not wait: it sends the due
	// alerts at once and leaves the window end as it is (ADR 0026). Zero
	// disables bundling.
	BundleWindow time.Duration
	// OnOutcome is called once per delivery attempt with the kind of the
	// channel and the outcome (OutcomeSent, OutcomeThrottled or OutcomeFailed),
	// for the delivery metrics (ADR 0025). It may be called concurrently. Nil
	// means none.
	OnOutcome func(kind, outcome string)
}

// Dispatcher delivers due outbox entries and repeats DOWN alerts (ADR 0014).
// It runs on the leader only; the FSM records every outcome.
type Dispatcher struct {
	opts    DispatcherOptions
	bundler BundleDeliverer // nil if the sender cannot bundle
	// wasLeader is the leadership seen by the last DeliverDue. Only DeliverDue
	// reads and writes it.
	wasLeader bool

	// mu guards reminders, the last reminder proposed per monitor.
	mu        sync.Mutex
	reminders map[string]reminderProposal

	// chMu guards channels, the in-memory rate and bundle state per channel.
	chMu     sync.Mutex
	channels map[channelKey]*channelState
}

// reminderProposal remembers the last reminder proposed for a monitor, so the
// same DOWN period is not proposed again on every tick.
type reminderProposal struct {
	sinceMs int64
	n       int64
}

// channelKey identifies a notification channel across tenants.
type channelKey struct {
	tenant  string
	channel string
}

// channelState is the in-memory delivery state of one channel. A change of
// leadership drops it (ADR 0023).
type channelState struct {
	tokens float64
	last   time.Time
	// blockedUntil is set when the receiver asked to wait (HTTP 429).
	blockedUntil time.Time
	// windowEnd ends the bundling window that opened with the last send. A
	// flush of DOWN alerts inside the window does not move it (ADR 0026).
	windowEnd time.Time
}

// take spends one token if one is available after refilling up to now.
func (s *channelState) take(now time.Time) bool {
	if now.After(s.last) {
		s.tokens = min(rateBurst, s.tokens+now.Sub(s.last).Seconds()*ratePerSecond)
		s.last = now
	}
	if s.tokens < 1 {
		return false
	}
	s.tokens--
	return true
}

// job is one message of a dispatch round: one outbox entry, or the entries of
// one Shoutrrr channel that go out as a bundle.
type job struct {
	key     channelKey
	ch      store.NotificationChannel
	entries []store.AlertDelivery
	// disabled marks a channel disabled by an earlier permanent failure: the
	// entries fail with the channel's error and no attempt is made.
	disabled bool
}

// result is the classified outcome of one send.
type result struct {
	delivered  bool
	msg        string
	permanent  bool
	retryAfter time.Duration
}

// NewDispatcher creates a dispatcher with defaults applied to opts.
func NewDispatcher(opts DispatcherOptions) *Dispatcher {
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = defaultMaxAttempts
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Jitter == nil {
		// Jitter only spreads retries; it is not security-relevant.
		opts.Jitter = rand.Float64
	}
	if opts.IsLeader == nil {
		opts.IsLeader = func() bool { return false }
	}
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}
	if opts.ReminderInterval <= 0 {
		opts.ReminderInterval = defaultReminderInterval
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = defaultConcurrency
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultBatchSize
	}
	bundler, _ := opts.Sender.(BundleDeliverer)
	return &Dispatcher{
		opts:      opts,
		bundler:   bundler,
		reminders: make(map[string]reminderProposal),
		channels:  make(map[channelKey]*channelState),
	}
}

// Run calls DeliverDue and EnqueueReminders on their intervals while this node
// is the leader, until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) {
	deliver := time.NewTicker(d.opts.Interval)
	defer deliver.Stop()
	remind := time.NewTicker(d.opts.ReminderInterval)
	defer remind.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deliver.C:
			if err := d.DeliverDue(ctx); err != nil && ctx.Err() == nil {
				d.opts.Logger.Warn("deliver due alerts", "error", err)
			}
		case <-remind.C:
			if !d.opts.IsLeader() {
				continue
			}
			if err := d.EnqueueReminders(ctx); err != nil && ctx.Err() == nil {
				d.opts.Logger.Warn("enqueue alert reminders", "error", err)
			}
		}
	}
}

// DeliverDue sends the outbox entries that are due and records each attempt.
// It returns after all of them are finished. Entries that the rate limit or a
// bundling window holds back stay due for a later round.
func (d *Dispatcher) DeliverDue(ctx context.Context) error {
	if !d.isLeader() {
		return nil
	}
	now := d.opts.Now()
	due, err := d.opts.State.DueAlerts(ctx, now.UnixMilli(), d.opts.BatchSize)
	if err != nil {
		return fmt.Errorf("load due alerts: %w", err)
	}
	d.run(ctx, d.plan(ctx, now, due))
	return nil
}

// isLeader reports whether this node leads. A change of leadership drops the
// in-memory channel state: the new leader starts with full buckets, and bundles
// pending on the old leader stay in the outbox for the new one (ADR 0023).
func (d *Dispatcher) isLeader() bool {
	leader := d.opts.IsLeader()
	if leader != d.wasLeader {
		d.wasLeader = leader
		d.chMu.Lock()
		d.channels = make(map[channelKey]*channelState)
		d.chMu.Unlock()
	}
	return leader
}

// plan decides which due entries go out in this round, per channel.
func (d *Dispatcher) plan(ctx context.Context, now time.Time, due []store.AlertDelivery) []job {
	var keys []channelKey
	groups := make(map[channelKey][]store.AlertDelivery)
	for _, a := range due {
		k := channelKey{tenant: a.TenantID, channel: a.ChannelID}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], a)
	}
	var jobs []job
	for _, k := range keys {
		ch, err := d.opts.State.GetChannel(ctx, k.tenant, k.channel)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// The channel was deleted; its entries are gone with it.
			d.chMu.Lock()
			delete(d.channels, k)
			d.chMu.Unlock()
			continue
		case err != nil:
			if ctx.Err() == nil {
				d.opts.Logger.Warn("load notification channel", "tenant", k.tenant, "channel", k.channel, "error", err)
			}
			continue
		}
		d.chMu.Lock()
		jobs = append(jobs, d.planChannel(now, k, ch, groups[k])...)
		d.chMu.Unlock()
	}
	return jobs
}

// planChannel decides the messages of one channel. The Retry-After block, the
// rate limit and the bundling window decide how many entries go out. It must
// be called with chMu held.
func (d *Dispatcher) planChannel(now time.Time, k channelKey, ch store.NotificationChannel, due []store.AlertDelivery) []job {
	// A channel disabled by an earlier permanent failure gets no more attempts
	// (FA-ALR-05). Its entries fail with the channel's error and use no token.
	if ch.State == store.ChannelDisabled {
		delete(d.channels, k)
		if len(due) == 0 {
			return nil
		}
		return []job{{key: k, ch: ch, entries: due, disabled: true}}
	}
	if len(due) == 0 {
		return nil
	}
	st := d.channels[k]
	if st == nil {
		st = &channelState{tokens: rateBurst, last: now}
		d.channels[k] = st
	}
	if now.Before(st.blockedUntil) {
		return nil
	}
	if d.opts.BundleWindow > 0 && d.bundler != nil && ch.Kind == store.ChannelShoutrrr {
		// The first alerts go out at once. Alerts due inside the window wait;
		// when it ends, all due alerts go out as one message (ADR 0023). A
		// bundle of one alert is sent as a normal message.
		if now.Before(st.windowEnd) {
			// A DOWN alert never waits for the window (ADR 0026): it sends all
			// due alerts of the channel now. The window keeps its end.
			if !anyDown(due) || !st.take(now) {
				return nil
			}
			return []job{{key: k, ch: ch, entries: due}}
		}
		if !st.take(now) {
			return nil
		}
		st.windowEnd = now.Add(d.opts.BundleWindow)
		return []job{{key: k, ch: ch, entries: due}}
	}
	var jobs []job
	for _, a := range due {
		if !st.take(now) {
			break // the rest waits for the next round
		}
		jobs = append(jobs, job{key: k, ch: ch, entries: []store.AlertDelivery{a}})
	}
	return jobs
}

// run sends the planned messages. The messages of one channel go one after
// the other, so a Retry-After block stops the rest of that channel. Channels
// run in parallel, up to Concurrency.
func (d *Dispatcher) run(ctx context.Context, jobs []job) {
	var order []channelKey
	byChannel := make(map[channelKey][]job)
	for _, j := range jobs {
		if _, ok := byChannel[j.key]; !ok {
			order = append(order, j.key)
		}
		byChannel[j.key] = append(byChannel[j.key], j)
	}
	sem := make(chan struct{}, d.opts.Concurrency)
	var wg sync.WaitGroup
	for _, k := range order {
		sem <- struct{}{}
		// Checked after the slot is free: a node that lost leadership while
		// the batch was running leaves the rest to the new leader (ADR 0014).
		if ctx.Err() != nil || !d.opts.IsLeader() {
			<-sem
			break
		}
		wg.Add(1)
		go func(list []job) {
			defer func() {
				<-sem
				wg.Done()
			}()
			for _, j := range list {
				if ctx.Err() != nil || !d.opts.IsLeader() || d.blocked(j.key) {
					return
				}
				d.runJob(ctx, j)
			}
		}(byChannel[k])
	}
	wg.Wait()
}

// anyDown reports whether one of the entries is a DOWN alert (ADR 0026). The
// event is read from the stored payload, once per entry.
func anyDown(entries []store.AlertDelivery) bool {
	for _, a := range entries {
		var head struct {
			Event string `json:"event"`
		}
		if json.Unmarshal(a.Payload, &head) == nil && head.Event == store.EventDown {
			return true
		}
	}
	return false
}

// blocked reports whether the receiver of a channel asked to wait.
func (d *Dispatcher) blocked(k channelKey) bool {
	d.chMu.Lock()
	defer d.chMu.Unlock()
	st := d.channels[k]
	return st != nil && d.opts.Now().Before(st.blockedUntil)
}

// block holds back the channel until the given instant.
func (d *Dispatcher) block(k channelKey, until time.Time) {
	d.chMu.Lock()
	defer d.chMu.Unlock()
	if st := d.channels[k]; st != nil && until.After(st.blockedUntil) {
		st.blockedUntil = until
	}
}

// runJob makes one message of a job and records the outcome of every entry in it.
func (d *Dispatcher) runJob(ctx context.Context, j job) {
	if j.disabled {
		for _, a := range j.entries {
			logger := d.entryLogger(a, a.Attempts+1)
			logger.Warn("alert not sent: notification channel is disabled", "error", j.ch.LastError)
			d.record(ctx, logger, a, a.Attempts+1, cluster.DeliveryOutcome{Permanent: true, Error: j.ch.LastError})
		}
		return
	}

	target, decErr := DecryptTarget(ctx, d.opts.Keyring, j.ch)
	var live []store.AlertDelivery
	var payloads []cluster.AlertPayload
	for _, a := range j.entries {
		attempt := a.Attempts + 1
		msg, permanent := "cannot decrypt channel secrets", false
		if decErr == nil {
			var p cluster.AlertPayload
			if err := json.Unmarshal(a.Payload, &p); err == nil {
				live = append(live, a)
				payloads = append(payloads, p)
				continue
			}
			msg, permanent = "invalid alert payload", true
		}
		d.finish(ctx, a, attempt, d.failure(attempt, permanent, msg, 0), msg)
		d.observe(j.ch.Kind, OutcomeFailed)
	}
	if len(live) == 0 {
		return
	}

	var sendErr error
	switch {
	case len(live) == 1:
		sendErr = d.opts.Sender.Send(ctx, target, payloads[0])
	case d.bundler != nil:
		sendErr = d.bundler.SendBundle(ctx, target, payloads)
	default:
		sendErr = &DeliveryError{Permanent: true, Msg: "this sender cannot bundle alerts"}
	}
	if sendErr != nil && ctx.Err() != nil {
		return // shutting down: not counted as an attempt, the entries stay pending
	}
	res := classify(sendErr)
	if res.retryAfter > 0 {
		d.block(j.key, d.opts.Now().Add(res.retryAfter))
	}
	for _, a := range live {
		attempt := a.Attempts + 1
		d.finish(ctx, a, attempt, d.outcome(attempt, res), res.msg)
		d.observe(j.ch.Kind, outcomeOf(res))
	}
}

// observe reports one delivery attempt to OnOutcome, if set.
func (d *Dispatcher) observe(kind, outcome string) {
	if d.opts.OnOutcome != nil {
		d.opts.OnOutcome(kind, outcome)
	}
}

// classify maps the error of a send to its result. The message is empty on
// success and free of secrets otherwise.
func classify(err error) result {
	if err == nil {
		return result{delivered: true}
	}
	res := result{msg: "delivery failed"}
	var de *DeliveryError
	if errors.As(err, &de) {
		res.msg, res.permanent, res.retryAfter = de.Msg, de.Permanent, de.RetryAfter
	}
	return res
}

// outcome turns the result of one attempt into the leader's decision.
func (d *Dispatcher) outcome(attempt int, res result) cluster.DeliveryOutcome {
	if res.delivered {
		return cluster.DeliveryOutcome{Delivered: true}
	}
	return d.failure(attempt, res.permanent, res.msg, res.retryAfter)
}

// finish logs and records the outcome of one attempt. An attempt cut short by
// shutdown is not recorded: it stays pending and is not counted.
func (d *Dispatcher) finish(ctx context.Context, a store.AlertDelivery, attempt int, out cluster.DeliveryOutcome, msg string) {
	logger := d.entryLogger(a, attempt)
	if out.Delivered {
		logger.Info("alert delivered")
	} else {
		if ctx.Err() != nil {
			return
		}
		logger.Warn("alert delivery failed", "error", msg, "permanent", out.Permanent, "give_up", out.GiveUp,
			"next_attempt_ms", out.NextAttemptMs)
	}
	d.record(ctx, logger, a, attempt, out)
}

func (d *Dispatcher) entryLogger(a store.AlertDelivery, attempt int) *slog.Logger {
	return d.opts.Logger.With("tenant", a.TenantID, "monitor", a.MonitorID, "channel", a.ChannelID,
		"alert", a.AlertKey, "attempt", attempt)
}

// record proposes the outcome of one attempt to the cluster.
func (d *Dispatcher) record(ctx context.Context, logger *slog.Logger, a store.AlertDelivery, attempt int, outcome cluster.DeliveryOutcome) {
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ackTimeout)
	defer cancel()
	_, err := d.opts.Cluster.Apply(ackCtx, cluster.NewAckAlertDelivery(a, uint32(attempt), outcome)) //nolint:gosec // attempts are small
	switch {
	case err == nil:
	case errors.Is(err, store.ErrConflict):
		logger.Debug("delivery already recorded", "error", err)
	default:
		logger.Warn("record delivery attempt", "error", err)
	}
}

// failure decides how a failed attempt continues: permanent failures stop,
// the last allowed attempt gives up, others retry after the backoff or the
// receiver's Retry-After if that is longer (ADR 0023).
func (d *Dispatcher) failure(attempt int, permanent bool, msg string, retryAfter time.Duration) cluster.DeliveryOutcome {
	switch {
	case permanent:
		return cluster.DeliveryOutcome{Permanent: true, Error: msg}
	case attempt >= d.opts.MaxAttempts:
		return cluster.DeliveryOutcome{GiveUp: true, Error: msg}
	default:
		wait := max(d.backoff(attempt), retryAfter)
		return cluster.DeliveryOutcome{Error: msg, NextAttemptMs: d.opts.Now().Add(wait).UnixMilli()}
	}
}

// backoff returns min(30 s * 2^(attempt-1), 1 h) scaled by a factor in [0.8, 1.2).
func (d *Dispatcher) backoff(attempt int) time.Duration {
	return backoffDelay(attempt, d.opts.Jitter())
}

// backoffDelay is the retry delay after attempt failed attempts. jitter is in
// [0,1) and selects the factor in [0.8, 1.2) (ADR 0014). It is shared with the
// degraded mode's local dispatcher.
func backoffDelay(attempt int, jitter float64) time.Duration {
	base := retryBase
	for i := 1; i < attempt && base < retryMax; i++ {
		base *= 2
	}
	base = min(base, retryMax)
	return time.Duration(float64(base) * (0.8 + 0.4*jitter))
}

// EnqueueReminders proposes the repeated DOWN alert of every monitor that is
// still DOWN longer than its resend interval (FA-ALR-02). Each reminder number
// is proposed once per DOWN period; the FSM ignores duplicates anyway, so
// proposing again after a restart is safe.
func (d *Dispatcher) EnqueueReminders(ctx context.Context) error {
	monitors, err := store.AllMonitors(ctx, d.opts.State)
	if err != nil {
		return fmt.Errorf("list monitors: %w", err)
	}
	nowMs := d.opts.Now().UnixMilli()
	periodUnit := time.Minute.Milliseconds()

	d.mu.Lock()
	defer d.mu.Unlock()
	next := make(map[string]reminderProposal)
	var errs []error
	for _, m := range monitors {
		if m.ResendIntervalMinutes <= 0 || m.State.Status != store.StatusDown || len(m.ChannelIDs) == 0 {
			continue
		}
		n := (nowMs - m.State.SinceMs) / (int64(m.ResendIntervalMinutes) * periodUnit)
		if n < 1 {
			continue
		}
		prev, seen := d.reminders[m.ID]
		if seen && prev.sinceMs == m.State.SinceMs && prev.n >= n {
			next[m.ID] = prev
			continue
		}
		cmd := cluster.NewEnqueueReminder(m.TenantID, m.ID, m.State.SinceMs, uint32(n)) //nolint:gosec // n is bounded by the age of the DOWN period
		_, err := d.opts.Cluster.Apply(ctx, cmd)
		switch {
		case err == nil, errors.Is(err, store.ErrConflict):
			// A conflict means the monitor left this DOWN period meanwhile; the
			// next round forgets it.
			next[m.ID] = reminderProposal{sinceMs: m.State.SinceMs, n: n}
		default:
			errs = append(errs, fmt.Errorf("propose reminder of monitor %s: %w", m.ID, err))
		}
	}
	d.reminders = next
	return errors.Join(errs...)
}
