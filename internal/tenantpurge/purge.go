// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package tenantpurge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/joinsider/uptimecluster/internal/store"
)

// Defaults of the purge job (ADR 0029).
const (
	DefaultInterval  = 30 * time.Second
	DefaultBatchSize = 1000
	defaultPause     = 10 * time.Millisecond
)

// Options configure a Job.
type Options struct {
	// State lists the tombstones of the deleted tenants.
	State store.State
	// Metrics purges the tenant rows of metrics.db.
	Metrics store.Metrics
	// Tombstones receives the deleted tenant IDs of every cycle.
	Tombstones *Tombstones
	// Snapshot requests a Raft snapshot, so that log entries with the deleted
	// configuration are truncated sooner (ADR 0029, "Grenze"). It is called
	// once per tenant that this node has not purged yet. Nil disables it.
	Snapshot func() error
	// Interval is the period of the cycles (default 30 s).
	Interval time.Duration
	// BatchSize is the number of rows per purge transaction (default 1000).
	BatchSize int
	// Pause is the wait between two batches, so that writers get the lock (default 10 ms).
	Pause time.Duration
	// Now is the wall clock for the purge time (default time.Now).
	Now    func() time.Time
	Logger *slog.Logger
}

// Job purges the metrics of deleted tenants on this node. Progress is kept in
// metrics.db (purged_tenants), so after a restart the job continues where it
// stopped: a tenant is marked only when a purge returns 0 rows.
type Job struct {
	opts        Options
	snapshotted map[string]bool
}

// New returns a job with the defaults applied to opts.
func New(opts Options) *Job {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBatchSize
	}
	if opts.Pause <= 0 {
		opts.Pause = defaultPause
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Job{opts: opts, snapshotted: map[string]bool{}}
}

// Run runs a cycle at once and then every Interval until ctx ends.
func (j *Job) Run(ctx context.Context) {
	ticker := time.NewTicker(j.opts.Interval)
	defer ticker.Stop()
	for {
		if err := j.Cycle(ctx); err != nil && ctx.Err() == nil {
			j.opts.Logger.Error("tenant purge cycle failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Cycle refreshes the tombstones and purges every deleted tenant that this
// node has not finished yet. A failing tenant does not stop the others.
func (j *Job) Cycle(ctx context.Context) error {
	deleted, err := j.opts.State.ListDeletedTenants(ctx)
	if err != nil {
		return fmt.Errorf("list deleted tenants: %w", err)
	}
	ids := make([]string, 0, len(deleted))
	for _, d := range deleted {
		ids = append(ids, d.ID)
	}
	j.opts.Tombstones.Replace(ids)

	purged, err := j.opts.Metrics.PurgedTenants(ctx)
	if err != nil {
		return fmt.Errorf("list purged tenants: %w", err)
	}
	done := make(map[string]bool, len(purged))
	for _, id := range purged {
		done[id] = true
	}
	var errs []error
	for _, id := range ids {
		if done[id] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := j.purge(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("purge tenant %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// purge deletes the metrics of one tenant in batches and marks it as purged.
func (j *Job) purge(ctx context.Context, tenantID string) error {
	log := j.opts.Logger.With("tenant_id", tenantID)
	if j.opts.Snapshot != nil && !j.snapshotted[tenantID] {
		j.snapshotted[tenantID] = true
		if err := j.opts.Snapshot(); err != nil {
			log.Warn("requesting a snapshot after a tenant deletion failed", "err", err)
		}
	}
	began := time.Now()
	log.Info("purging metrics of deleted tenant")
	var rows int64
	for {
		n, err := j.opts.Metrics.PurgeTenant(ctx, tenantID, j.opts.BatchSize)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
		rows += n
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(j.opts.Pause):
		}
	}
	if err := j.opts.Metrics.MarkTenantPurged(ctx, tenantID, j.opts.Now().UnixMilli()); err != nil {
		return fmt.Errorf("mark purged: %w", err)
	}
	log.Info("metrics of deleted tenant purged", "rows", rows, "duration", time.Since(began))
	return nil
}
