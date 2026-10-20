// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package uptime

import (
	"context"
	"slices"

	"github.com/joinsider/uptimecluster/internal/store"
)

// Calculator computes buckets from the raw data.
type Calculator struct {
	State   store.State
	Metrics store.Metrics
}

// heartbeats loads a monitor's heartbeats with ts in [from, to), optionally of
// one node, grouped by node and ordered by time.
func (c *Calculator) heartbeats(ctx context.Context, m store.Monitor, nodeID string, from, to int64) (map[string][]store.Heartbeat, error) {
	out := map[string][]store.Heartbeat{}
	q := store.HeartbeatQuery{MonitorID: m.ID, NodeID: nodeID, FromMs: from, ToMs: to - 1, Limit: 5000}
	for {
		page, err := c.Metrics.ListHeartbeats(ctx, m.TenantID, q)
		if err != nil {
			return nil, err
		}
		for _, hb := range page.Items {
			out[hb.NodeID] = append(out[hb.NodeID], hb)
		}
		if page.Next == nil {
			return out, nil
		}
		q.After = page.Next
	}
}

// Compute returns the rollups of [start, end): the consensus view first (if
// opts.NodeID is ""), then one per node that reported, ordered by node ID.
// For the consensus alone without latency, heartbeats are not read.
func (c *Calculator) Compute(ctx context.Context, m store.Monitor, start, end int64, opts ComputeOptions) ([]store.Rollup, error) {
	bucket := store.Rollup{TenantID: m.TenantID, MonitorID: m.ID, StartMs: start, BucketMs: opts.BucketMs}
	var out []store.Rollup

	var byNode map[string][]store.Heartbeat
	if opts.Latency || !opts.ConsensusOnly {
		var err error
		if byNode, err = c.heartbeats(ctx, m, opts.NodeID, start-coverMs(m), end); err != nil {
			return nil, err
		}
	}

	// The status history is needed for the consensus and for the maintenance
	// time of the node rollups.
	var changes []store.StatusChange
	if opts.NodeID == "" || !opts.ConsensusOnly {
		var err error
		if changes, err = c.State.StatusHistory(ctx, m.TenantID, m.ID, start, end); err != nil {
			return nil, err
		}
	}

	if opts.NodeID == "" {
		r := bucket
		r.UpMs, r.DownMs, r.MaintMs = consensusTimes(changes, start, end)
		var all []int
		for _, hbs := range byNode {
			all = append(all, okLatencies(hbs, start, end)...)
		}
		setLatency(&r, latencyOf(all))
		out = append(out, finish(r, end))
	}
	if opts.ConsensusOnly {
		return out, nil
	}

	nodes := make([]string, 0, len(byNode))
	for n := range byNode {
		nodes = append(nodes, n)
	}
	slices.Sort(nodes)
	maint := maintenanceSpans(changes, start, end)
	for _, n := range nodes {
		hbs := byNode[n]
		r := bucket
		r.NodeID = n
		r.UpMs, r.DownMs, r.MaintMs = nodeUpDown(hbs, maint, start, end, coverMs(m))
		setLatency(&r, latencyOf(okLatencies(hbs, start, end)))
		out = append(out, finish(r, end))
	}
	return out, nil
}

// ComputeOptions select what Compute returns.
type ComputeOptions struct {
	BucketMs int64
	// NodeID restricts the result to one node; "" includes the consensus.
	NodeID string
	// ConsensusOnly skips per-node rollups.
	ConsensusOnly bool
	// Latency reads heartbeats for latency even if only the consensus is needed.
	Latency bool
}

func setLatency(r *store.Rollup, l latencyStats) {
	r.LatencyCount, r.LatencyAvg, r.LatencyP95 = l.count, l.avg, l.p95
}

// finish sets the unknown time of a bucket that ends at end.
func finish(r store.Rollup, end int64) store.Rollup {
	r.UnknownMs = max(0, end-r.StartMs-r.UpMs-r.DownMs-r.MaintMs)
	return r
}
