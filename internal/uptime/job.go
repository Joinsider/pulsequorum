// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package uptime

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/joinsider/pulsequorum/internal/store"
)

// Watermark names in metrics.db.
const (
	wmHour       = "rollup_hour" // end of the last rolled-up hour
	wmDay        = "rollup_day"  // end of the last rolled-up day
	wmCompaction = "compaction"  // end of the last compacted hour
)

const (
	hourMs = store.BucketHour
	dayMs  = store.BucketDay
	// grace lets checks that started before the end of an hour finish and
	// be stored before the hour is rolled up.
	grace = 2 * time.Minute
	// CompactAfter is how long raw heartbeats are kept in full (FA-UPT-04).
	CompactAfter = 48 * time.Hour
	// compactLookback is how far compaction looks for a node's previous
	// heartbeat; without one the first heartbeat of the hour is kept.
	compactLookback = 24 * time.Hour
	// maxHoursPerRun bounds the work of one run while catching up.
	maxHoursPerRun = 24
	// maxReplicationHoldBack bounds how long rollups and compaction wait for
	// heartbeats of other voters that arrive late after a partition (FA-REP-03).
	// Without the bound, a dead voter would stop all rollups.
	maxReplicationHoldBack = time.Hour
	// Retention deletes in small batches with pauses (FA-UPT-06).
	retentionBatch = 5000
	retentionPause = 50 * time.Millisecond
	retentionEvery = time.Hour
)

// Retention configures how long data is kept (FA-UPT-05). Zero keeps forever.
type Retention struct {
	Raw    time.Duration // heartbeats and latency minutes
	Hourly time.Duration
	Daily  time.Duration
}

// DefaultRetention follows Pflichtenheft 7.3.
var DefaultRetention = Retention{Raw: 30 * 24 * time.Hour, Hourly: 365 * 24 * time.Hour}

// JobOptions configure a Job.
type JobOptions struct {
	State   store.State
	Metrics store.Metrics
	// Vacuum returns freed pages of metrics.db to the file system.
	Vacuum func(ctx context.Context) error
	Logger *slog.Logger
	// Compaction enables FA-UPT-04 (RAW_COMPACTION).
	Compaction bool
	Retention  Retention
	// Now returns the wall clock; tests replace it.
	Now func() time.Time
	// ReplicatedUntil returns the time up to which the heartbeats of all peers
	// are replicated here (FA-REP-03). Hours after it are held back. Nil means
	// no hold-back.
	ReplicatedUntil func() time.Time
}

// Job maintains rollups, compaction and retention of metrics.db. It runs
// locally on every voter; the results are deterministic (FA-UPT-03).
type Job struct {
	opts          JobOptions
	calc          Calculator
	lastRetention time.Time
}

// NewJob creates the job.
func NewJob(opts JobOptions) *Job {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Job{opts: opts, calc: Calculator{State: opts.State, Metrics: opts.Metrics}}
}

// Run calls RunOnce every interval until ctx ends.
func (j *Job) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := j.RunOnce(ctx); err != nil && ctx.Err() == nil {
			j.opts.Logger.Error("uptime maintenance failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunOnce rolls up finished hours and days, compacts raw data older than
// CompactAfter and applies retention once per hour.
func (j *Job) RunOnce(ctx context.Context) error {
	now := j.opts.Now()
	monitors, err := store.AllMonitors(ctx, j.opts.State)
	if err != nil {
		return err
	}
	if err := j.rollup(ctx, monitors, now); err != nil {
		return err
	}
	if j.opts.Compaction {
		if err := j.compact(ctx, monitors, now); err != nil {
			return err
		}
	}
	if now.Sub(j.lastRetention) >= retentionEvery {
		if err := j.retain(ctx, now); err != nil {
			return err
		}
		j.lastRetention = now
	}
	return nil
}

func floorTo(ms, bucket int64) int64 { return ms - ((ms%bucket)+bucket)%bucket }

// replicationLimit returns the end of the newest hour that may be rolled up or
// compacted with respect to replication, or math.MaxInt64 without hold-back.
func (j *Job) replicationLimit(now time.Time) int64 {
	if j.opts.ReplicatedUntil == nil {
		return math.MaxInt64
	}
	return holdBackLimit(now, j.opts.ReplicatedUntil())
}

// holdBackLimit returns the end of the newest hour that is final for all
// voters: the replicated-until time minus grace, floored to an hour. The
// replicated-until time is never older than maxReplicationHoldBack, so that a
// voter which stopped replicating does not stop the rollups.
func holdBackLimit(now, replicatedUntil time.Time) int64 {
	safe := now.Add(-maxReplicationHoldBack)
	if replicatedUntil.After(safe) {
		safe = replicatedUntil
	}
	return floorTo(safe.Add(-grace).UnixMilli(), hourMs)
}

// watermark returns a watermark, initializing it to def on first use.
func (j *Job) watermark(ctx context.Context, name string, def int64) (int64, error) {
	v, err := j.opts.Metrics.Watermark(ctx, name)
	if err != nil || v != 0 {
		return v, err
	}
	return def, j.opts.Metrics.SetWatermark(ctx, name, def)
}

func (j *Job) rollup(ctx context.Context, monitors []store.Monitor, now time.Time) error {
	start := floorTo(now.Add(-CompactAfter).UnixMilli(), hourMs)
	hourWM, err := j.watermark(ctx, wmHour, start)
	if err != nil {
		return err
	}
	dayWM, err := j.watermark(ctx, wmDay, floorTo(hourWM, dayMs))
	if err != nil {
		return err
	}

	limit := min(floorTo(now.Add(-grace).UnixMilli(), hourMs), j.replicationLimit(now))
	for n := 0; hourWM+hourMs <= limit && n < maxHoursPerRun; n++ {
		from, to := hourWM, hourWM+hourMs
		var rollups []store.Rollup
		for _, m := range monitors {
			if m.CreatedMs >= to {
				continue
			}
			rs, err := j.calc.Compute(ctx, m, from, to, ComputeOptions{BucketMs: hourMs, Latency: true})
			if err != nil {
				return fmt.Errorf("roll up monitor %s at %d: %w", m.ID, from, err)
			}
			rollups = append(rollups, rs...)
		}
		if err := j.opts.Metrics.PutRollups(ctx, rollups, map[string]int64{wmHour: to}); err != nil {
			return err
		}
		hourWM = to
	}

	// Days are the sum of their hours.
	for dayWM+dayMs <= floorTo(hourWM, dayMs) {
		from, to := dayWM, dayWM+dayMs
		var rollups []store.Rollup
		for _, m := range monitors {
			if m.CreatedMs >= to {
				continue
			}
			rs, err := j.dayFromHours(ctx, m, from, to)
			if err != nil {
				return fmt.Errorf("roll up day %d of monitor %s: %w", from, m.ID, err)
			}
			rollups = append(rollups, rs...)
		}
		if err := j.opts.Metrics.PutRollups(ctx, rollups, map[string]int64{wmDay: to}); err != nil {
			return err
		}
		dayWM = to
	}
	return nil
}

func (j *Job) dayFromHours(ctx context.Context, m store.Monitor, from, to int64) ([]store.Rollup, error) {
	hours, err := j.opts.Metrics.ListRollups(ctx, m.TenantID, store.RollupQuery{
		MonitorID: m.ID, AllNodes: true, BucketMs: hourMs, FromMs: from, ToMs: to,
	})
	if err != nil {
		return nil, err
	}
	var out []store.Rollup
	for i := 0; i < len(hours); {
		k := i
		for k < len(hours) && hours[k].NodeID == hours[i].NodeID {
			k++
		}
		day := store.Rollup{TenantID: m.TenantID, MonitorID: m.ID, NodeID: hours[i].NodeID, BucketMs: dayMs, StartMs: from}
		out = append(out, finish(merge(hours[i:k], day), to))
		i = k
	}
	return out, nil
}

// compact replaces raw heartbeats of rolled-up hours older than CompactAfter
// by latency minutes; heartbeats that change a node's status stay (FA-UPT-04).
func (j *Job) compact(ctx context.Context, monitors []store.Monitor, now time.Time) error {
	hourWM, err := j.opts.Metrics.Watermark(ctx, wmHour)
	if err != nil {
		return err
	}
	// Starts where the rollups started: older raw data is left to retention.
	// Hours still open for late replicated heartbeats are not compacted: a
	// late heartbeat would otherwise be compacted away or missing from the
	// hour's rollup.
	limit := min(hourWM, floorTo(now.Add(-CompactAfter).UnixMilli(), hourMs), j.replicationLimit(now))
	compWM, err := j.watermark(ctx, wmCompaction, limit)
	if err != nil {
		return err
	}
	for n := 0; compWM+hourMs <= limit && n < maxHoursPerRun; n++ {
		from, to := compWM, compWM+hourMs
		for _, m := range monitors {
			if m.CreatedMs >= to {
				continue
			}
			if err := j.compactMonitor(ctx, m, from, to); err != nil {
				return fmt.Errorf("compact monitor %s at %d: %w", m.ID, from, err)
			}
		}
		if err := j.opts.Metrics.SetWatermark(ctx, wmCompaction, to); err != nil {
			return err
		}
		compWM = to
	}
	return nil
}

func (j *Job) compactMonitor(ctx context.Context, m store.Monitor, from, to int64) error {
	// The previous heartbeat decides whether the first one changes the status.
	// The lookback is long because older hours are compacted already and hold
	// only the rows that changed the status.
	byNode, err := j.calc.heartbeats(ctx, m, "", from-max(coverMs(m), compactLookback.Milliseconds()), to)
	if err != nil {
		return err
	}
	minutes, deleted := compactRows(m, byNode, from)
	if len(minutes) == 0 && len(deleted) == 0 {
		return nil
	}
	return j.opts.Metrics.CompactHeartbeats(ctx, minutes, deleted)
}

// compactRows computes the latency minutes of heartbeats at or after from and
// the heartbeats to delete: those with the same status as the node's previous
// heartbeat. Without a previous heartbeat the first one is kept.
func compactRows(m store.Monitor, byNode map[string][]store.Heartbeat, from int64) ([]store.LatencyMinute, []store.HeartbeatKey) {
	var minutes []store.LatencyMinute
	var deleted []store.HeartbeatKey
	for node, hbs := range byNode {
		nm, nd := compactNodeRows(m, node, hbs, from)
		minutes = append(minutes, nm...)
		deleted = append(deleted, nd...)
	}
	return minutes, deleted
}

func compactNodeRows(m store.Monitor, node string, hbs []store.Heartbeat, from int64) ([]store.LatencyMinute, []store.HeartbeatKey) {
	var minutes []store.LatencyMinute
	var deleted []store.HeartbeatKey
	var prev store.CheckStatus
	known := false
	var ok []int
	cur := store.LatencyMinute{TenantID: m.TenantID, MonitorID: m.ID, NodeID: node, MinuteMs: -1}
	for _, hb := range hbs {
		if hb.TsMs < from {
			prev, known = hb.Status, true
			continue
		}
		minute := floorTo(hb.TsMs, 60_000)
		if minute != cur.MinuteMs {
			minutes = flushLatencyMinute(minutes, cur, ok)
			cur = store.LatencyMinute{TenantID: m.TenantID, MonitorID: m.ID, NodeID: node, MinuteMs: minute}
			ok = ok[:0]
		}
		if hb.Status == store.CheckDown {
			cur.ErrCount++
		} else {
			ok = append(ok, hb.LatencyMs)
		}
		if known && hb.Status == prev {
			deleted = append(deleted, store.HeartbeatKey{NodeID: hb.NodeID, Incarnation: hb.Incarnation, Seq: hb.Seq})
		}
		prev, known = hb.Status, true
	}
	minutes = flushLatencyMinute(minutes, cur, ok)
	return minutes, deleted
}

func flushLatencyMinute(minutes []store.LatencyMinute, cur store.LatencyMinute, ok []int) []store.LatencyMinute {
	if cur.MinuteMs < 0 {
		return minutes
	}
	if len(ok) > 0 {
		l := latencyOf(ok)
		cur.Min, cur.Max, cur.Avg, cur.P95 = minOf(ok), maxOf(ok), l.avg, l.p95
	}
	cur.OKCount = len(ok)
	return append(minutes, cur)
}

func minOf(v []int) int {
	m := v[0]
	for _, x := range v[1:] {
		m = min(m, x)
	}
	return m
}

func maxOf(v []int) int {
	m := v[0]
	for _, x := range v[1:] {
		m = max(m, x)
	}
	return m
}

// retain deletes expired data in batches and returns the space (FA-UPT-05/06).
func (j *Job) retain(ctx context.Context, now time.Time) error {
	type target struct {
		name   string
		keep   time.Duration
		delete func(ctx context.Context, before int64, batch int) (int64, error)
	}
	m := j.opts.Metrics
	r := j.opts.Retention
	targets := []target{
		{"heartbeats", r.Raw, m.DeleteHeartbeatsBefore},
		{"latency minutes", r.Raw, m.DeleteLatencyMinutesBefore},
		{"hourly rollups", r.Hourly, func(ctx context.Context, before int64, batch int) (int64, error) {
			return m.DeleteRollupsBefore(ctx, hourMs, before, batch)
		}},
		{"daily rollups", r.Daily, func(ctx context.Context, before int64, batch int) (int64, error) {
			return m.DeleteRollupsBefore(ctx, dayMs, before, batch)
		}},
	}
	var total int64
	for _, t := range targets {
		if t.keep <= 0 {
			continue
		}
		before := now.Add(-t.keep).UnixMilli()
		for {
			n, err := t.delete(ctx, before, retentionBatch)
			if err != nil {
				return fmt.Errorf("retention of %s: %w", t.name, err)
			}
			total += n
			if n < retentionBatch {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retentionPause):
			}
		}
	}
	if total > 0 {
		j.opts.Logger.Info("retention deleted expired data", "rows", total)
		if j.opts.Vacuum != nil {
			return j.opts.Vacuum(ctx)
		}
	}
	return nil
}
