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

// Applier proposes commands to the cluster. *cluster.Node satisfies it.
type Applier interface {
	Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error)
}

// Deliverer sends one alert to one target. *Sender satisfies it.
type Deliverer interface {
	Send(ctx context.Context, t Target, p cluster.AlertPayload) error
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
}

// Dispatcher delivers due outbox entries and repeats DOWN alerts (ADR 0014).
// It runs on the leader only; the FSM records every outcome.
type Dispatcher struct {
	opts DispatcherOptions

	// mu guards reminders, the last reminder proposed per monitor.
	mu        sync.Mutex
	reminders map[string]reminderProposal
}

// reminderProposal remembers the last reminder proposed for a monitor, so the
// same DOWN period is not proposed again on every tick.
type reminderProposal struct {
	sinceMs int64
	n       int64
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
	return &Dispatcher{opts: opts, reminders: make(map[string]reminderProposal)}
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
			if !d.opts.IsLeader() {
				continue
			}
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
// It returns after all of them are finished.
func (d *Dispatcher) DeliverDue(ctx context.Context) error {
	due, err := d.opts.State.DueAlerts(ctx, d.opts.Now().UnixMilli(), d.opts.BatchSize)
	if err != nil {
		return fmt.Errorf("load due alerts: %w", err)
	}
	sem := make(chan struct{}, d.opts.Concurrency)
	var wg sync.WaitGroup
	for _, a := range due {
		sem <- struct{}{}
		// Checked after the slot is free: a node that lost leadership while
		// the batch was running leaves the rest to the new leader (ADR 0014).
		if ctx.Err() != nil || !d.opts.IsLeader() {
			<-sem
			break
		}
		wg.Add(1)
		go func() {
			defer func() {
				<-sem
				wg.Done()
			}()
			d.deliver(ctx, a)
		}()
	}
	wg.Wait()
	return nil
}

// deliver makes one attempt for one outbox entry and proposes its outcome.
func (d *Dispatcher) deliver(ctx context.Context, a store.AlertDelivery) {
	attempt := a.Attempts + 1
	logger := d.opts.Logger.With("tenant", a.TenantID, "monitor", a.MonitorID, "channel", a.ChannelID,
		"alert", a.AlertKey, "attempt", attempt)

	ch, err := d.opts.State.GetChannel(ctx, a.TenantID, a.ChannelID)
	if errors.Is(err, store.ErrNotFound) {
		return // the channel was deleted; the entry stays untouched
	}
	if err != nil {
		if ctx.Err() == nil {
			logger.Warn("load notification channel", "error", err)
		}
		return
	}

	// A channel disabled by an earlier permanent failure gets no more attempts
	// (FA-ALR-05). The entry fails with the channel's own error.
	if ch.State == store.ChannelDisabled {
		logger.Warn("alert not sent: notification channel is disabled", "error", ch.LastError)
		d.record(ctx, logger, a, attempt, cluster.DeliveryOutcome{Permanent: true, Error: ch.LastError})
		return
	}

	outcome, msg := d.attempt(ctx, ch, a)
	if msg != "" && ctx.Err() != nil {
		return // shutting down: not counted as an attempt, the entry stays pending
	}
	if outcome.Delivered {
		logger.Info("alert delivered")
	} else {
		logger.Warn("alert delivery failed", "error", msg, "permanent", outcome.Permanent, "give_up", outcome.GiveUp,
			"next_attempt_ms", outcome.NextAttemptMs)
	}
	d.record(ctx, logger, a, attempt, outcome)
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

// attempt sends the alert and maps the result to the leader's decision. The
// returned message is empty on success and free of secrets otherwise.
func (d *Dispatcher) attempt(ctx context.Context, ch store.NotificationChannel, a store.AlertDelivery) (cluster.DeliveryOutcome, string) {
	attempt := a.Attempts + 1
	target, err := DecryptTarget(ctx, d.opts.Keyring, ch)
	if err != nil {
		return d.failure(attempt, false, "cannot decrypt channel secrets"), "cannot decrypt channel secrets"
	}
	var p cluster.AlertPayload
	if err := json.Unmarshal(a.Payload, &p); err != nil {
		return d.failure(attempt, true, "invalid alert payload"), "invalid alert payload"
	}

	err = d.opts.Sender.Send(ctx, target, p)
	if err == nil {
		return cluster.DeliveryOutcome{Delivered: true}, ""
	}
	msg := "delivery failed"
	permanent := false
	var de *DeliveryError
	if errors.As(err, &de) {
		msg, permanent = de.Msg, de.Permanent
	}
	return d.failure(attempt, permanent, msg), msg
}

// failure decides how a failed attempt continues: permanent failures stop,
// the last allowed attempt gives up, others retry after a backoff.
func (d *Dispatcher) failure(attempt int, permanent bool, msg string) cluster.DeliveryOutcome {
	switch {
	case permanent:
		return cluster.DeliveryOutcome{Permanent: true, Error: msg}
	case attempt >= d.opts.MaxAttempts:
		return cluster.DeliveryOutcome{GiveUp: true, Error: msg}
	default:
		now := d.opts.Now()
		return cluster.DeliveryOutcome{Error: msg, NextAttemptMs: now.Add(d.backoff(attempt)).UnixMilli()}
	}
}

// backoff returns min(30 s * 2^(attempt-1), 1 h) scaled by a factor in [0.8, 1.2).
func (d *Dispatcher) backoff(attempt int) time.Duration {
	base := retryBase
	for i := 1; i < attempt && base < retryMax; i++ {
		base *= 2
	}
	base = min(base, retryMax)
	return time.Duration(float64(base) * (0.8 + 0.4*d.opts.Jitter()))
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
