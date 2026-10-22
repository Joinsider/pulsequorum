// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package uptime

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite"
)

// TestConsensusTotalsMatchStats checks that the batched totals equal the
// per-monitor Stats for monitors with rolled-up, live, unknown and recently
// created hours, with changes at and between hour boundaries.
func TestConsensusTotalsMatchStats(t *testing.T) {
	ctx := context.Background()
	state, err := sqlite.OpenState(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	metrics, err := sqlite.OpenMetrics(ctx, t.TempDir(), sqlite.MetricsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metrics.Close() })

	mk := func(id string, created time.Duration) store.Monitor {
		return store.Monitor{TenantID: "t", ID: id, Name: id, Type: "http", Config: []byte(`{}`), IntervalSeconds: 60,
			TimeoutSeconds: 10, RecoveryThreshold: 1, Assignment: store.Assignment{Mode: "all"}, ConsensusPolicy: "majority",
			CreatedMs: at(created), UpdatedMs: at(created)}
	}
	monitors := []store.Monitor{
		mk("m1", -72*time.Hour),
		mk("m2", -330*time.Minute), // created inside the window
		mk("m3", -72*time.Hour),
		mk("m4", -72*time.Hour), // heartbeats but no status history
		mk("m5", -72*time.Hour), // status history but no heartbeats
	}
	history := map[string][]store.StatusChange{
		"m1": {{SinceMs: at(-72 * time.Hour), Status: store.StatusUp}, {SinceMs: at(-10 * time.Hour), Status: store.StatusDown},
			{SinceMs: at(-570 * time.Minute), Status: store.StatusUp}, {SinceMs: at(-3 * time.Hour), Status: store.StatusMaintenance}},
		"m2": {{SinceMs: at(-330 * time.Minute), Status: store.StatusUp}, {SinceMs: at(-2 * time.Hour), Status: store.StatusDown}},
		"m3": {{SinceMs: at(-72 * time.Hour), Status: store.StatusUp}, {SinceMs: at(-48 * time.Hour), Status: store.StatusDown},
			{SinceMs: at(-24 * time.Hour), Status: store.StatusUp}},
		"m5": {{SinceMs: at(-30 * time.Hour), Status: store.StatusDegraded}, {SinceMs: at(-20 * time.Hour), Status: store.StatusPartial},
			{SinceMs: at(-10 * time.Hour), Status: store.StatusUp}, {SinceMs: at(-70 * time.Minute), Status: store.StatusMaintenance}},
	}
	// setHistory records status changes of existing monitors.
	setHistory := func(changes map[string][]store.StatusChange) {
		err := state.Update(ctx, func(tx store.StateTx) error {
			for id, cs := range changes {
				for _, c := range cs {
					if err := tx.PutMonitorState(ctx, "t", id, store.MonitorState{Status: c.Status, Confirmed: true, SinceMs: c.SinceMs}); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	err = state.Update(ctx, func(tx store.StateTx) error {
		if err := tx.PutTenant(ctx, store.Tenant{ID: "t", Name: "t", Plan: "default"}); err != nil {
			return err
		}
		for _, m := range monitors {
			if err := tx.PutMonitor(ctx, m, 0); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	setHistory(history)

	// Heartbeats of m1 and m4 from -49h to now, DOWN in [-10h, -9h30m) for m1.
	var hbs []store.Heartbeat
	seq := uint64(0)
	for _, id := range []string{"m1", "m4"} {
		for ts := at(-49 * time.Hour); ts < at(0); ts += 60_000 {
			seq++
			status := store.CheckUp
			if id == "m1" && ts >= at(-10*time.Hour) && ts < at(-570*time.Minute) {
				status = store.CheckDown
			}
			hbs = append(hbs, store.Heartbeat{NodeID: "n1", Incarnation: "i", Seq: seq, TenantID: "t", MonitorID: id,
				TsMs: ts, Status: status, LatencyMs: 40 + int(seq%30)})
		}
	}
	if _, err := metrics.InsertHeartbeats(ctx, hbs); err != nil {
		t.Fatal(err)
	}

	// Roll up until the watermark stops moving. The hours after the last
	// rollup are computed from the status history.
	now1 := t0
	job := NewJob(JobOptions{State: state, Metrics: metrics, Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return now1 }})
	for range 5 {
		if err := job.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	setHistory(map[string][]store.StatusChange{
		"m1": {{SinceMs: at(-30 * time.Minute), Status: store.StatusDown}},
		"m3": {{SinceMs: at(5 * time.Minute), Status: store.StatusDown}},
	})

	svc := &Service{State: state, Metrics: metrics}
	var sawUp, sawDown, sawMaint, sawUnknown bool
	for _, now := range []time.Time{now1, now1.Add(10 * time.Minute)} {
		for _, r := range []Range{Range24h, Range7d} {
			totals, err := svc.ConsensusTotals(ctx, "t", monitors, r, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(totals) != len(monitors) {
				t.Fatalf("totals of %d monitors, want %d", len(totals), len(monitors))
			}
			for _, m := range monitors {
				stats, err := svc.Stats(ctx, m, "", r, now, false)
				if err != nil {
					t.Fatal(err)
				}
				got := totals[m.ID]
				if got != stats.Total {
					t.Errorf("%s at %s, %s: batch = %+v, Stats = %+v", m.ID, now.Format(time.RFC3339), r, got, stats.Total)
				}
				sawUp = sawUp || got.UpMs > 0
				sawDown = sawDown || got.DownMs > 0
				sawMaint = sawMaint || got.MaintMs > 0
				sawUnknown = sawUnknown || got.UnknownMs > 0
			}
		}
	}
	if !sawUp || !sawDown || !sawMaint || !sawUnknown {
		t.Fatalf("scenario lacks variety: up %v, down %v, maintenance %v, unknown %v", sawUp, sawDown, sawMaint, sawUnknown)
	}

	if got, err := svc.ConsensusTotals(ctx, "other", monitors, Range24h, now1); err == nil {
		t.Errorf("monitors of tenant t accepted for tenant other: %+v", got)
	}
	if _, err := svc.ConsensusTotals(ctx, "t", monitors, Range30d, now1); err == nil {
		t.Error("daily range accepted")
	}
	if got, err := svc.ConsensusTotals(ctx, "t", nil, Range24h, now1); err != nil || len(got) != 0 {
		t.Errorf("no monitors = %+v, %v", got, err)
	}
}

// TestStatusWindow checks that the slice of a longer history equals what
// StatusHistory(from, to) returns for it.
func TestStatusWindow(t *testing.T) {
	changes := []store.StatusChange{change(10, store.StatusUp), change(20, store.StatusDown), change(30, store.StatusUp)}
	tests := []struct {
		from, to int64
		want     []store.StatusChange
	}{
		{0, 15, changes[:1]},
		{0, 100, changes},
		{12, 25, changes[:2]},
		{20, 20, nil},
		{25, 40, changes[1:]},
		{5, 10, nil},
		{30, 31, changes[2:]},
		{31, 40, changes[2:]},
	}
	for _, tt := range tests {
		got := statusWindow(changes, tt.from, tt.to)
		if !slices.Equal(got, tt.want) {
			t.Errorf("statusWindow([%d, %d)) = %+v, want %+v", tt.from, tt.to, got, tt.want)
		}
	}
}
