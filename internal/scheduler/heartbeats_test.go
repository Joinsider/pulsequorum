// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package scheduler

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/checker"
	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite"
)

func TestHeartbeatWriter(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics, err := sqlite.OpenMetrics(ctx, dir, sqlite.MetricsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = metrics.Close() }()

	writeThree := func() {
		w, err := NewHeartbeatWriter(ctx, metrics, "node-1", log)
		if err != nil {
			t.Fatal(err)
		}
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { w.Run(runCtx); close(done) }()
		m := store.Monitor{TenantID: "t", ID: "m"}
		for i := range 3 {
			w.Record(runCtx, Outcome{Monitor: m, Started: time.UnixMilli(int64(1000 * (i + 1))),
				Result: checker.Result{Status: checker.StatusDown, LatencyMs: 7, ErrorClass: checker.ClassDNS, Message: "nx",
					Details: map[string]any{"rcode": "NXDOMAIN"}}})
		}
		cancel() // the remaining queue is flushed on shutdown
		<-done
	}
	writeThree()
	writeThree() // a restarted writer continues the sequence

	page, err := metrics.ListHeartbeats(ctx, "t", store.HeartbeatQuery{MonitorID: "m"})
	if err != nil || len(page.Items) != 6 {
		t.Fatalf("heartbeats = %d, %v", len(page.Items), err)
	}
	seqs := map[uint64]bool{}
	for _, hb := range page.Items {
		seqs[hb.Seq] = true
		if hb.NodeID != "node-1" || hb.Incarnation != metrics.Incarnation() || hb.Status != store.CheckDown ||
			hb.ErrorClass != "dns" || string(hb.Details) != `{"rcode":"NXDOMAIN"}` {
			t.Fatalf("heartbeat = %+v", hb)
		}
	}
	for seq := uint64(1); seq <= 6; seq++ {
		if !seqs[seq] {
			t.Errorf("seq %d missing; sequence must continue without gaps or reuse", seq)
		}
	}
}
