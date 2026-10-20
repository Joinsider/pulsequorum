// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package aggregator turns check results into the consensus status of each
// monitor and records status changes and incidents via Raft (FA-CON-03/06,
// FA-INC-01, Konzept 9.1). In phase 1 the evaluation uses this node's results
// only; the consensus over several nodes follows in phase 3.
package aggregator

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/checker"
	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/scheduler"
	"github.com/joinsider/uptimecluster/internal/store"
)

const (
	queueSize    = 1024
	applyTimeout = 5 * time.Second
)

// Applier proposes Raft commands.
type Applier interface {
	Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error)
}

// Options configure an Aggregator.
type Options struct {
	State   store.State
	Metrics store.Metrics
	Cluster Applier
	// NodeID is this node; its heartbeats rebuild the retry count after a restart.
	NodeID string
	// Maintenance reports whether m is in a maintenance window at the time of
	// the outcome. Such outcomes are ignored (FA-MNT-02, FA-ALR-13). Nil means none.
	Maintenance func(m store.Monitor, at time.Time) bool
	Logger      *slog.Logger
}

// Aggregator evaluates outcomes in the order they arrive.
type Aggregator struct {
	opts  Options
	queue chan scheduler.Outcome

	mu       sync.Mutex
	trackers map[string]*tracker
}

var _ scheduler.Sink = (*Aggregator)(nil)

// New creates an Aggregator; call Run to start it.
func New(opts Options) *Aggregator {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Aggregator{opts: opts, queue: make(chan scheduler.Outcome, queueSize), trackers: map[string]*tracker{}}
}

// Record implements scheduler.Sink. It blocks only if the queue is full.
func (a *Aggregator) Record(ctx context.Context, o scheduler.Outcome) {
	select {
	case a.queue <- o:
	case <-ctx.Done():
	}
}

// Sync forgets monitors that no longer exist; it is called by the reconciler.
func (a *Aggregator) Sync(monitors []store.Monitor) {
	keep := make(map[string]bool, len(monitors))
	for _, m := range monitors {
		keep[m.ID] = true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id := range a.trackers {
		if !keep[id] {
			delete(a.trackers, id)
		}
	}
}

// Run evaluates queued outcomes until ctx ends.
func (a *Aggregator) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case o := <-a.queue:
			a.process(ctx, o)
		}
	}
}

func (a *Aggregator) process(ctx context.Context, o scheduler.Outcome) {
	m := o.Monitor
	a.mu.Lock()
	defer a.mu.Unlock()

	// A check during maintenance says nothing about the monitor: it proposes
	// nothing (FA-MNT-02). The cached tracker is dropped, so that the streaks
	// of the time before the maintenance do not count afterwards; the next
	// evaluation reloads the state and replays only results since its since.
	if a.opts.Maintenance != nil && a.opts.Maintenance(m, o.Started) {
		delete(a.trackers, m.ID)
		return
	}

	t, err := a.tracker(ctx, m, o.Started)
	if err != nil {
		if ctx.Err() == nil {
			a.opts.Logger.Error("loading monitor state failed", "tenant", m.TenantID, "monitor", m.ID, "err", err)
		}
		return
	}
	if t == nil {
		return // deleted
	}
	at := o.Started.UnixMilli()
	t.observe(o.Result, at)
	to, since, change := t.next(o.Result.Status, at, m.Retries, m.RecoveryThreshold)
	if !change {
		return
	}

	c := cluster.StateChange{TenantID: m.TenantID, MonitorID: m.ID, From: t.status, To: to, SinceMs: since}
	if to == store.StatusDown {
		c.Incident = &store.Incident{
			ID:            uuid.NewString(),
			Cause:         t.lastFailure.Message,
			ErrorClass:    string(t.lastFailure.ErrorClass),
			AffectedNodes: []string{a.opts.NodeID},
		}
	}
	applyCtx, cancel := context.WithTimeout(ctx, applyTimeout)
	_, err = a.opts.Cluster.Apply(applyCtx, cluster.NewSetMonitorState(c))
	cancel()

	log := a.opts.Logger.With("tenant", m.TenantID, "monitor", m.ID, "from", c.From, "to", to)
	switch {
	case err == nil:
		t.status = to
		log.Info("monitor status changed")
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound):
		// Paused, resumed or deleted meanwhile: reload on the next result.
		delete(a.trackers, m.ID)
		log.Debug("status change superseded", "err", err)
	case errors.Is(err, cluster.ErrNotApplied):
		// Not the leader; the next result retries.
		log.Debug("status change not applied", "err", err)
	default:
		log.Warn("recording status change failed; retrying with the next result", "err", err)
	}
}

// tracker returns the evaluation state of m, loading it when the monitor is
// new to this node or its configuration changed. It returns nil if the monitor
// no longer exists.
func (a *Aggregator) tracker(ctx context.Context, m store.Monitor, started time.Time) (*tracker, error) {
	if t := a.trackers[m.ID]; t != nil && t.revision == m.Revision {
		return t, nil
	}
	cur, err := a.opts.State.GetMonitor(ctx, m.TenantID, m.ID)
	if errors.Is(err, store.ErrNotFound) {
		delete(a.trackers, m.ID)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t := &tracker{revision: m.Revision, status: cur.State.Status}

	// Replay this node's latest results since the last status change, so that
	// a restart does not reset the retry count (NFA-REL-03).
	hbs, err := a.opts.Metrics.LatestHeartbeats(ctx, m.TenantID, m.ID, a.opts.NodeID, max(m.Retries+1, m.RecoveryThreshold)+1)
	if err != nil {
		return nil, err
	}
	for i := len(hbs) - 1; i >= 0; i-- {
		hb := hbs[i]
		if hb.TsMs < cur.State.SinceMs || hb.TsMs >= started.UnixMilli() {
			continue
		}
		t.observe(checker.Result{
			Status:     checker.Status(hb.Status),
			Message:    hb.Message,
			ErrorClass: checker.ErrorClass(hb.ErrorClass),
		}, hb.TsMs)
	}
	a.trackers[m.ID] = t
	return t, nil
}
