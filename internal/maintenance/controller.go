// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package maintenance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

const (
	defaultInterval = 15 * time.Second
	applyTimeout    = 5 * time.Second
	// maxExtensions bounds the walk over chained or overlapping windows.
	maxExtensions = 100
)

// Applier proposes Raft commands. *cluster.Node satisfies it.
type Applier interface {
	Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error)
}

// ControllerOptions configure a Controller. Zero values select the defaults,
// except for State, Cluster and IsLeader, which should be set.
type ControllerOptions struct {
	State   store.State
	Cluster Applier
	// IsLeader reports whether this node may propose commands; only the leader reconciles.
	IsLeader func() bool
	Logger   *slog.Logger
	// Now is the clock used for the reconciliation (default time.Now).
	Now func() time.Time
	// Interval is the period of Refresh and Reconcile (default 15 s).
	Interval time.Duration
}

// Controller keeps the MAINTENANCE status of monitors in line with the
// maintenance windows (FA-MNT-02). Every node caches the compiled windows so
// that the aggregator can ask InMaintenance; only the leader proposes status
// changes.
type Controller struct {
	opts ControllerOptions

	mu      sync.RWMutex
	windows []compiledWindow

	// refreshMu serializes Refresh and guards warned.
	refreshMu sync.Mutex
	// warned holds the invalid windows already reported by Refresh. Entries of
	// windows that disappeared are dropped.
	warned map[warnKey]bool
}

// warnKey identifies one revision of an invalid window.
type warnKey struct {
	tenant   string
	window   string
	revision uint64
}

// compiledWindow is an active window with its validated schedule.
type compiledWindow struct {
	window   store.MaintenanceWindow
	schedule *Schedule
}

// NewController creates a Controller with defaults applied to opts.
func NewController(opts ControllerOptions) *Controller {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.IsLeader == nil {
		opts.IsLeader = func() bool { return false }
	}
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}
	return &Controller{opts: opts, warned: map[warnKey]bool{}}
}

// Run refreshes the cached windows and, on the leader, reconciles the monitor
// states, immediately and then every Interval, until ctx ends.
func (c *Controller) Run(ctx context.Context) {
	ticker := time.NewTicker(c.opts.Interval)
	defer ticker.Stop()
	for {
		c.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Controller) tick(ctx context.Context) {
	if err := c.Refresh(ctx); err != nil && ctx.Err() == nil {
		c.opts.Logger.Warn("refresh maintenance windows", "err", err)
	}
	if !c.opts.IsLeader() {
		return
	}
	if err := c.Reconcile(ctx); err != nil && ctx.Err() == nil {
		c.opts.Logger.Warn("reconcile maintenance", "err", err)
	}
}

// Refresh loads the maintenance windows of all tenants, keeps the active ones
// and compiles their schedules. Invalid schedules are skipped with a warning.
// On error the previous set stays in effect. An invalid window is reported
// once per revision, as it is refreshed every interval.
func (c *Controller) Refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	all, err := c.opts.State.ListAllMaintenanceWindows(ctx)
	if err != nil {
		return fmt.Errorf("list maintenance windows: %w", err)
	}
	compiled := make([]compiledWindow, 0, len(all))
	invalid := make(map[warnKey]bool)
	for _, w := range all {
		if !w.Active {
			continue
		}
		s, err := Compile(w.Schedule)
		if err != nil {
			key := warnKey{tenant: w.TenantID, window: w.ID, revision: w.Revision}
			if !c.warned[key] {
				c.opts.Logger.Warn("skipping invalid maintenance window", "tenant", w.TenantID, "window", w.ID, "err", err)
			}
			invalid[key] = true
			continue
		}
		compiled = append(compiled, compiledWindow{window: w, schedule: s})
	}
	c.warned = invalid
	c.mu.Lock()
	c.windows = compiled
	c.mu.Unlock()
	return nil
}

// InMaintenance reports whether an active window applies to m at the instant
// at. It uses the windows cached by Refresh and is safe for concurrent use.
func (c *Controller) InMaintenance(m store.Monitor, at time.Time) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, cw := range c.windows {
		if !cw.window.Covers(m) {
			continue
		}
		if _, _, ok := cw.schedule.Occurrence(at); ok {
			return true
		}
	}
	return false
}

// Reconcile proposes the status changes that bring every monitor (except the
// paused ones) in line with the cached windows at the current time: monitors
// inside a window enter MAINTENANCE, monitors whose last window has ended
// leave it. Only the leader may call it.
func (c *Controller) Reconcile(ctx context.Context) error {
	monitors, err := store.AllMonitors(ctx, c.opts.State)
	if err != nil {
		return fmt.Errorf("list monitors: %w", err)
	}
	now := c.opts.Now()
	c.mu.RLock()
	windows := c.windows // Refresh replaces the slice, so it is never modified in place
	c.mu.RUnlock()

	for _, m := range monitors {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if m.State.Status == store.StatusPaused {
			continue
		}
		c.reconcileMonitor(ctx, m, now, windows)
	}
	return nil
}

func (c *Controller) reconcileMonitor(ctx context.Context, m store.Monitor, now time.Time, windows []compiledWindow) {
	var (
		active   int
		earliest int64
	)
	for _, cw := range windows {
		if !cw.window.Covers(m) {
			continue
		}
		start, _, ok := cw.schedule.Occurrence(now)
		if !ok {
			continue
		}
		active++
		if ms := start.UnixMilli(); active == 1 || ms < earliest {
			earliest = ms
		}
	}

	status := m.State.Status
	logger := c.opts.Logger.With("tenant", m.TenantID, "monitor", m.ID)
	switch {
	case active > 0 && status != store.StatusMaintenance:
		c.apply(ctx, logger, cluster.StateChange{
			TenantID: m.TenantID, MonitorID: m.ID,
			From: status, To: store.StatusMaintenance, SinceMs: earliest,
		}, active, "monitor entered maintenance")

	case active == 0 && status == store.StatusMaintenance:
		c.apply(ctx, logger, cluster.StateChange{
			TenantID: m.TenantID, MonitorID: m.ID,
			From: store.StatusMaintenance, To: store.StatusPending, SinceMs: exitTime(m, now, windows),
		}, 0, "monitor left maintenance")
	}
}

// exitTime returns when maintenance of m ends. Windows that touch or overlap
// the end of the current maintenance extend it, so chained windows end at the
// union end. Without such an extension the maintenance ends now, unless the
// recorded start lies in the past, which happens after a leader change.
func exitTime(m store.Monitor, now time.Time, windows []compiledWindow) int64 {
	start := m.State.SinceMs
	e := start
	for range maxExtensions {
		next := e
		for _, cw := range windows {
			if !cw.window.Covers(m) {
				continue
			}
			_, end, ok := cw.schedule.Occurrence(time.UnixMilli(e))
			if ok && end.UnixMilli() > next {
				next = end.UnixMilli()
			}
		}
		if next == e {
			break
		}
		e = next
	}
	if e > start && e <= now.UnixMilli() {
		return e
	}
	return now.UnixMilli()
}

// apply proposes one status change. A conflict means the state changed
// meanwhile; the next reconciliation decides again.
func (c *Controller) apply(ctx context.Context, logger *slog.Logger, change cluster.StateChange, windows int, msg string) {
	applyCtx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	_, err := c.opts.Cluster.Apply(applyCtx, cluster.NewSetMonitorState(change))
	switch {
	case err == nil:
		logger.Info(msg, "from", change.From, "to", change.To, "windows", windows)
	case errors.Is(err, store.ErrConflict):
		logger.Debug("maintenance transition superseded", "from", change.From, "to", change.To, "err", err)
	default:
		logger.Warn("maintenance transition failed", "from", change.From, "to", change.To, "err", err)
	}
}
