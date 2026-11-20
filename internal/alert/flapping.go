// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package alert

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/store"
)

// flappingQuiet is how long a flapping monitor must keep its status before the
// leader ends the period (FA-ALR-06, ADR 0023). It matches the window in which
// the FSM counts changes.
const flappingQuiet = 30 * time.Minute

// defaultFlappingInterval is the period of the flapping check (default 1 min).
const defaultFlappingInterval = time.Minute

// FlappingWatcherOptions configures the leader job that ends flapping periods.
type FlappingWatcherOptions struct {
	State store.State
	// Cluster proposes the commands; a *cluster.Node satisfies it.
	Cluster Applier
	// IsLeader reports whether this node may propose commands; only the leader runs the check.
	IsLeader func() bool
	// Now is the clock of the leader (default time.Now).
	Now    func() time.Time
	Logger *slog.Logger
	// Interval is the period of Check (default 1 min).
	Interval time.Duration
}

// FlappingWatcher proposes ClearMonitorFlapping for monitors that have been
// quiet for flappingQuiet. The FSM records the end, so a leader change does not
// lose it (ADR 0023, 4).
type FlappingWatcher struct {
	opts FlappingWatcherOptions
}

// NewFlappingWatcher creates a watcher with defaults applied to opts.
func NewFlappingWatcher(opts FlappingWatcherOptions) *FlappingWatcher {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.IsLeader == nil {
		opts.IsLeader = func() bool { return false }
	}
	if opts.Interval <= 0 {
		opts.Interval = defaultFlappingInterval
	}
	return &FlappingWatcher{opts: opts}
}

// Run calls Check on every interval while this node is the leader, until ctx ends.
func (w *FlappingWatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !w.opts.IsLeader() {
				continue
			}
			if err := w.Check(ctx); err != nil && ctx.Err() == nil {
				w.opts.Logger.Warn("check flapping monitors", "error", err)
			}
		}
	}
}

// Check proposes the end of every flapping period that has been quiet long
// enough. A conflict means the period changed meanwhile, and not_enabled means
// the cluster is below command level 4; both are normal and not errors.
func (w *FlappingWatcher) Check(ctx context.Context) error {
	monitors, err := store.AllMonitors(ctx, w.opts.State)
	if err != nil {
		return fmt.Errorf("list monitors: %w", err)
	}
	quietBefore := w.opts.Now().Add(-flappingQuiet).UnixMilli()
	var errs []error
	for _, m := range monitors {
		st := m.State
		if !st.Flapping || st.SinceMs > quietBefore || st.FlappingSinceMs > quietBefore {
			continue
		}
		_, err := w.opts.Cluster.Apply(ctx, cluster.NewClearMonitorFlapping(m.TenantID, m.ID, st.FlappingSinceMs))
		var cmdErr *cluster.CommandError
		switch {
		case err == nil:
			w.opts.Logger.Info("flapping ended", "tenant", m.TenantID, "monitor", m.ID)
		case errors.Is(err, store.ErrConflict):
			w.opts.Logger.Debug("flapping changed meanwhile", "tenant", m.TenantID, "monitor", m.ID)
		case errors.As(err, &cmdErr) && cmdErr.Code == cluster.CodeNotEnabled:
			w.opts.Logger.Debug("flapping not enabled yet", "tenant", m.TenantID, "monitor", m.ID)
		default:
			errs = append(errs, fmt.Errorf("end flapping of monitor %s: %w", m.ID, err))
		}
	}
	return errors.Join(errs...)
}
