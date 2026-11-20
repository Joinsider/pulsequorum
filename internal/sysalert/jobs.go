// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sysalert

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/store"
)

// DefaultCheckpointInterval is the period of the FSM checkpoints (ADR 0025, section 4).
const DefaultCheckpointInterval = time.Hour

// jobTick is the period in which the leader jobs check their conditions.
const jobTick = 5 * time.Second

// TenantState reads a tenant (store.State satisfies it).
type TenantState interface {
	GetTenant(ctx context.Context, id string) (store.Tenant, error)
}

// JobOptions configure Jobs. IsLeader, CommandLevel, State and Cluster are required.
type JobOptions struct {
	IsLeader     func() bool
	CommandLevel func(ctx context.Context) (uint32, error)
	State        TenantState
	Cluster      Applier
	// CheckpointInterval is the period of the checkpoints; zero means DefaultCheckpointInterval.
	CheckpointInterval time.Duration
	// Now is the clock of this node; nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Jobs are the leader's housekeeping for the system alerts (ADR 0025): the
// reserved system tenant, which is created once per leadership at command
// level 6, and the hourly FSM checkpoint, which changes no data.
type Jobs struct {
	opts JobOptions

	leader     bool
	tenantDone bool
	// lastCheckpoint is the time of the last checkpoint proposal; it is kept
	// across leaderships.
	lastCheckpoint time.Time
}

// NewJobs creates the leader jobs with defaults applied to opts.
func NewJobs(opts JobOptions) *Jobs {
	if opts.CheckpointInterval <= 0 {
		opts.CheckpointInterval = DefaultCheckpointInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Jobs{opts: opts}
}

// Run ticks until ctx ends.
func (j *Jobs) Run(ctx context.Context) {
	ticker := time.NewTicker(jobTick)
	defer ticker.Stop()
	for {
		j.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick does the work that is due: on a leader at command level 6 the system
// tenant once per leadership, and a checkpoint if the last one is an interval
// old or none was proposed yet.
func (j *Jobs) Tick(ctx context.Context) {
	if !j.opts.IsLeader() {
		j.leader, j.tenantDone = false, false
		return
	}
	if !j.leader {
		j.leader, j.tenantDone = true, false
	}
	level, err := j.opts.CommandLevel(ctx)
	if err != nil {
		j.opts.Logger.Warn("system jobs: read command level", "err", err)
		return
	}
	if level < cluster.SystemAlertCommandLevel {
		return
	}
	if !j.tenantDone {
		j.ensureSystemTenant(ctx)
	}
	now := j.opts.Now()
	if j.lastCheckpoint.IsZero() || now.Sub(j.lastCheckpoint) >= j.opts.CheckpointInterval {
		j.lastCheckpoint = now
		callCtx, cancel := context.WithTimeout(ctx, applyTimeout)
		_, err := j.opts.Cluster.Apply(callCtx, cluster.NewCheckpoint())
		cancel()
		if err != nil && ctx.Err() == nil {
			j.opts.Logger.Warn("system jobs: checkpoint not recorded", "err", err)
		}
	}
}

// ensureSystemTenant creates the system tenant if it is missing. A failure is
// retried on the next tick.
func (j *Jobs) ensureSystemTenant(ctx context.Context) {
	_, err := j.opts.State.GetTenant(ctx, store.SystemTenantID)
	switch {
	case err == nil:
		j.tenantDone = true
		return
	case !errors.Is(err, store.ErrNotFound):
		j.opts.Logger.Warn("system jobs: read system tenant", "err", err)
		return
	}
	cmd, err := cluster.NewPutSystemTenant(cluster.SystemTenantRequestID)
	if err != nil {
		j.opts.Logger.Warn("system jobs: build system tenant", "err", err)
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, applyTimeout)
	_, err = j.opts.Cluster.Apply(callCtx, cmd)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			j.opts.Logger.Warn("system jobs: create system tenant, retrying", "err", err)
		}
		return
	}
	j.opts.Logger.Info("system tenant created")
	j.tenantDone = true
}
