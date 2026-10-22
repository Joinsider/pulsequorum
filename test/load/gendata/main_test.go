// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/node"
	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite"
)

// TestRunBackfillsRollupsAndCompacts runs the generator for three days on a
// data directory with two monitors and checks the stored history.
func TestRunBackfillsRollupsAndCompacts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	const tenant = "default"
	end := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	const day = 24 * time.Hour

	// A voter's data directory: node ID and state.db with two monitors that were
	// created ten days ago. state.db is not written by the generator.
	nodeID := uuid.NewString()
	if err := os.WriteFile(filepath.Join(dir, node.IDFile), []byte(nodeID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := sqlite.OpenState(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	created := end.Add(-10 * day).UnixMilli()
	err = state.Update(ctx, func(tx store.StateTx) error {
		if err := tx.PutTenant(ctx, store.Tenant{ID: tenant, Name: tenant, Plan: "default"}); err != nil {
			return err
		}
		for _, id := range []string{"m1", "m2"} {
			m := store.Monitor{TenantID: tenant, ID: id, Name: id, Type: "http", Config: []byte(`{}`), IntervalSeconds: 60,
				TimeoutSeconds: 10, RecoveryThreshold: 1, Assignment: store.Assignment{Mode: "all"},
				ConsensusPolicy: "majority", CreatedMs: created, UpdatedMs: created}
			if err := tx.PutMonitor(ctx, m, 0); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.DiscardHandler)
	sum, err := run(ctx, options{DataDir: dir, Days: 3, Tenant: tenant, DownRate: 0.002, Seed: 1}, end, io.Discard, log)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Monitors != 2 {
		t.Fatalf("monitors = %d, want 2", sum.Monitors)
	}
	// Two monitors, 3 days of 60 s checks; the first check has a random offset.
	if want := int64(2 * 3 * 1440); sum.Rows > want || sum.Rows < want-2 {
		t.Fatalf("rows = %d, want about %d", sum.Rows, want)
	}

	metrics, err := sqlite.OpenMetrics(ctx, dir, sqlite.MetricsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = metrics.Close() }()

	// The stream continues after the stored seq: the last seq equals the rows.
	last, err := metrics.LastSeq(ctx, nodeID, metrics.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	if last != uint64(sum.Rows) {
		t.Errorf("last seq %d, want %d", last, sum.Rows)
	}

	startMs := end.Add(-3 * day).UnixMilli()
	// Hourly node rollups exist for the complete hours (71 of 72 are complete at end).
	rollups, err := metrics.ListRollups(ctx, tenant, store.RollupQuery{MonitorID: "m1", AllNodes: true,
		BucketMs: store.BucketHour, FromMs: startMs, ToMs: end.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	nodeHours := 0
	for _, r := range rollups {
		if r.NodeID == nodeID && r.LatencyCount > 0 {
			nodeHours++
		}
	}
	if nodeHours < 70 {
		t.Errorf("hourly node rollups = %d, want at least 70", nodeHours)
	}

	// Raw heartbeats older than 48 h are compacted: only the first heartbeat
	// and the status changes remain, far fewer than the 1380 checks in the window.
	oldRaw := countHeartbeats(t, metrics, "m1", startMs, end.Add(-49*time.Hour).UnixMilli())
	if oldRaw > 138 {
		t.Errorf("%d raw heartbeats older than 48 h left, want compacted", oldRaw)
	}
	// The last 47 h are kept in full (about 2820 checks per monitor).
	recent := countHeartbeats(t, metrics, "m1", end.Add(-47*time.Hour).UnixMilli(), end.UnixMilli())
	if recent < 2800 {
		t.Errorf("%d raw heartbeats in the last 47 h, want all of them", recent)
	}

	// state.db is only read: the monitors are unchanged.
	state, err = sqlite.OpenState(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = state.Close() }()
	page, err := state.ListMonitors(ctx, tenant, store.MonitorQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].CreatedMs != created {
		t.Errorf("state.db monitors changed: %d monitors, created %d", len(page.Items), page.Items[0].CreatedMs)
	}
}

func countHeartbeats(t *testing.T, metrics store.Metrics, monitorID string, fromMs, toMs int64) int {
	t.Helper()
	q := store.HeartbeatQuery{MonitorID: monitorID, FromMs: fromMs, ToMs: toMs - 1, Limit: 5000}
	n := 0
	for {
		page, err := metrics.ListHeartbeats(context.Background(), "default", q)
		if err != nil {
			t.Fatal(err)
		}
		n += len(page.Items)
		if page.Next == nil {
			return n
		}
		q.After = page.Next
	}
}

func TestRunRejectsExistingHistory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, node.IDFile), []byte(uuid.NewString()), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := options{DataDir: dir, Days: 1, Tenant: "default", DownRate: 0, Seed: 1}
	if _, err := run(ctx, opts, time.Now(), io.Discard, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("run without state.db succeeded")
	}
}

func TestOptionsValidate(t *testing.T) {
	base := options{DataDir: "d", Days: 1, Tenant: "default", DownRate: 0.002}
	for name, mutate := range map[string]func(*options){
		"no data dir": func(o *options) { o.DataDir = "" },
		"no days":     func(o *options) { o.Days = 0 },
		"no tenant":   func(o *options) { o.Tenant = "" },
		"rate 1":      func(o *options) { o.DownRate = 1 },
		"rate < 0":    func(o *options) { o.DownRate = -0.1 },
	} {
		o := base
		mutate(&o)
		if err := o.validate(); err == nil {
			t.Errorf("%s: validate succeeded", name)
		}
	}
	if err := base.validate(); err != nil {
		t.Errorf("valid options: %v", err)
	}
}
