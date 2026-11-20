// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"net/http"
	"time"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/uptime"
)

// GetUptime returns the uptime of a monitor over a range, for the consensus
// or one node (FA-UPT-01).
func (h *Handlers) GetUptime(ctx context.Context, req gen.GetUptimeRequestObject) (gen.GetUptimeResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	if !req.Params.Range.Valid() {
		return nil, &Problem{Status: http.StatusBadRequest, Title: "Bad Request", Detail: "range must be 24h, 7d, 30d or 365d"}
	}
	m, err := h.monitor(ctx, p, req.MonitorId)
	if err != nil {
		return nil, err
	}
	nodeID := ""
	if req.Params.Node != nil {
		nodeID = *req.Params.Node
	}
	stats, err := h.uptime.Stats(ctx, m, nodeID, uptime.Range(req.Params.Range), h.deps.Now(), true)
	if err != nil {
		return nil, err
	}

	t := stats.Total
	out := gen.UptimeStats{
		Range:              gen.UptimeStatsRange(stats.Range),
		UpSeconds:          t.UpMs / 1000,
		DownSeconds:        t.DownMs / 1000,
		MaintenanceSeconds: t.MaintMs / 1000,
		UnknownSeconds:     t.UnknownMs / 1000,
		Buckets:            make([]gen.UptimeBucket, 0, len(stats.Buckets)),
	}
	if nodeID != "" {
		out.NodeId = &nodeID
	}
	if pct, ok := uptime.Percent(t); ok {
		out.UptimePercent = &pct
	}
	if t.LatencyCount > 0 {
		out.LatencyAvgMs, out.LatencyP95Ms = &t.LatencyAvg, &t.LatencyP95
	}
	for _, b := range stats.Buckets {
		out.Buckets = append(out.Buckets, toAPIBucket(b))
	}
	return gen.GetUptime200JSONResponse(out), nil
}

func toAPIBucket(b store.Rollup) gen.UptimeBucket {
	out := gen.UptimeBucket{
		Start:              time.UnixMilli(b.StartMs).UTC(),
		UpSeconds:          b.UpMs / 1000,
		DownSeconds:        b.DownMs / 1000,
		MaintenanceSeconds: b.MaintMs / 1000,
		UnknownSeconds:     b.UnknownMs / 1000,
	}
	if b.LatencyCount > 0 {
		avg := b.LatencyAvg
		out.LatencyAvgMs = &avg
	}
	return out
}
