// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/store"
)

// Syncer receives the current set of monitors.
type Syncer interface {
	Sync(monitors []store.Monitor)
}

// AssignmentSyncer hands Target only the monitors that node NodeID runs
// (ADR 0017). Registry reads the node registry on every Sync; if that fails,
// Target keeps the previous monitors.
type AssignmentSyncer struct {
	Target   Syncer
	NodeID   string
	Assigner Assigner
	Registry func() ([]store.Node, error)
	Log      *slog.Logger
}

// Sync implements Syncer.
func (s AssignmentSyncer) Sync(monitors []store.Monitor) {
	registry, err := s.Registry()
	if err != nil {
		s.Log.Error("reading the node registry failed, keeping the current monitors", "err", err)
		return
	}
	mine := make([]store.Monitor, 0, len(monitors))
	for _, m := range monitors {
		if s.Assigner.Runs(m, registry, s.NodeID) {
			mine = append(mine, m)
		}
	}
	s.Target.Sync(mine)
}

// Capabilities returns the capability function of an Assigner for the checkers
// of r: the capability a checker requires (checker.CapabilityRequirer), or ""
// if it requires none or the type is unknown.
func Capabilities(r *checker.Registry) func(monitorType string) string {
	return func(monitorType string) string {
		c, err := r.Lookup(monitorType)
		if err != nil {
			return ""
		}
		if req, ok := c.(checker.CapabilityRequirer); ok {
			return req.RequiredCapability()
		}
		return ""
	}
}

// Reconcile keeps the targets (the scheduler and everything that tracks
// monitors) in line with state.db: it loads all monitors when changes signals
// and at least every interval, so a configuration change is active within
// seconds (FA-SCH-06). A target that runs checks is wrapped in an
// AssignmentSyncer, so it gets the monitors of its node only (ADR 0017).
func Reconcile(ctx context.Context, state store.State, changes <-chan struct{}, interval time.Duration, log *slog.Logger, targets ...Syncer) {
	load := func() {
		monitors, err := store.AllMonitors(ctx, state)
		if err != nil {
			if ctx.Err() == nil {
				log.Error("loading monitors for the scheduler failed", "err", err)
			}
			return
		}
		for _, t := range targets {
			t.Sync(monitors)
		}
	}
	load()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-changes:
		case <-ticker.C:
		}
		load()
	}
}
