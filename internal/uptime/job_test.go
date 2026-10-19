// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package uptime

import (
	"context"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite"
)

// t0 is a UTC midnight; the scenario runs around it.
var t0 = time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)

func at(d time.Duration) int64 { return t0.Add(d).UnixMilli() }

type world struct {
	t       *testing.T
	ctx     context.Context
	state   *sqlite.StateStore
	monitor store.Monitor
}

// newWorld creates a monitor (interval 60 s) with this consensus history:
// UP from -49h, DOWN in [-10h, -9h30m), UP afterwards.
func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	state, err := sqlite.OpenState(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	m := store.Monitor{TenantID: "t", ID: "m1", Name: "web", Type: "http", Config: []byte(`{}`), IntervalSeconds: 60,
		TimeoutSeconds: 10, RecoveryThreshold: 1, Assignment: store.Assignment{Mode: "all"}, ConsensusPolicy: "majority",
		CreatedMs: at(-72 * time.Hour), UpdatedMs: at(-72 * time.Hour)}
	err = state.Update(ctx, func(tx store.StateTx) error {
		if err := tx.PutTenant(ctx, store.Tenant{ID: "t", Name: "t", Plan: "default"}); err != nil {
			return err
		}
		if err := tx.PutMonitor(ctx, m, 0); err != nil {
			return err
		}
		for _, c := range []store.StatusChange{
			{SinceMs: at(-49 * time.Hour), Status: store.StatusUp},
			{SinceMs: at(-10 * time.Hour), Status: store.StatusDown},
			{SinceMs: at(-570 * time.Minute), Status: store.StatusUp},
		} {
			if err := tx.PutMonitorState(ctx, "t", "m1", store.MonitorState{Status: c.Status, Confirmed: true, SinceMs: c.SinceMs}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if m, err = state.GetMonitor(ctx, "t", "m1"); err != nil {
		t.Fatal(err)
	}
	return &world{t: t, ctx: ctx, state: state, monitor: m}
}

// newMetrics stores node n1's heartbeats every minute from -49h to +4h: DOWN
// in [-10h, -9h30m) and [-46h30m, -46h28m), latency 100 ms otherwise; and n2
// from -2h, always up with 50 ms.
func (w *world) newMetrics() *sqlite.MetricsStore {
	w.t.Helper()
	metrics, err := sqlite.OpenMetrics(w.ctx, w.t.TempDir(), sqlite.MetricsOptions{})
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = metrics.Close() })
	var hbs []store.Heartbeat
	var seq1, seq2 uint64
	for ts := at(-49 * time.Hour); ts < at(4*time.Hour); ts += 60_000 {
		s, lat := store.CheckUp, 100
		if (ts >= at(-10*time.Hour) && ts < at(-570*time.Minute)) || (ts >= at(-2790*time.Minute) && ts < at(-2788*time.Minute)) {
			s, lat = store.CheckDown, 0
		}
		seq1++
		hbs = append(hbs, store.Heartbeat{NodeID: "n1", Incarnation: "i1", Seq: seq1, TenantID: "t", MonitorID: "m1", TsMs: ts, Status: s, LatencyMs: lat})
		if ts >= at(-2*time.Hour) {
			seq2++
			hbs = append(hbs, store.Heartbeat{NodeID: "n2", Incarnation: "i2", Seq: seq2, TenantID: "t", MonitorID: "m1", TsMs: ts, Status: store.CheckUp, LatencyMs: 50})
		}
	}
	for i := 0; i < len(hbs); i += 2000 {
		if _, err := metrics.InsertHeartbeats(w.ctx, hbs[i:min(i+2000, len(hbs))]); err != nil {
			w.t.Fatal(err)
		}
	}
	return metrics
}

func (w *world) job(metrics *sqlite.MetricsStore, now *time.Time, retention Retention) *Job {
	return NewJob(JobOptions{State: w.state, Metrics: metrics, Logger: slog.New(slog.DiscardHandler), Compaction: true,
		Retention: retention, Now: func() time.Time { return *now },
		Vacuum: func(ctx context.Context) error { return metrics.DB().IncrementalVacuum(ctx, 0) }})
}

// runUntilIdle repeats RunOnce until the watermarks stop moving (catch-up is
// limited per run).
func runUntilIdle(t *testing.T, j *Job, metrics store.Metrics) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		before, _ := metrics.Watermark(ctx, wmHour)
		comp, _ := metrics.Watermark(ctx, wmCompaction)
		if err := j.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		after, _ := metrics.Watermark(ctx, wmHour)
		comp2, _ := metrics.Watermark(ctx, wmCompaction)
		if before == after && comp == comp2 {
			return
		}
	}
	t.Fatal("job did not settle")
}

func rollup(t *testing.T, m store.Metrics, node string, bucket int64, start int64) store.Rollup {
	t.Helper()
	rs, err := m.ListRollups(context.Background(), "t", store.RollupQuery{MonitorID: "m1", NodeID: node, BucketMs: bucket, FromMs: start, ToMs: start + 1})
	if err != nil || len(rs) != 1 {
		t.Fatalf("rollup %s@%d: %v, %v", node, start, rs, err)
	}
	return rs[0]
}

const (
	minute = int64(60_000)
	hour   = 60 * minute
)

func TestRollups(t *testing.T) {
	w := newWorld(t)
	metrics := w.newMetrics()
	svc := &Service{State: w.state, Metrics: metrics}

	now := t0.Add(65 * time.Minute)
	live, err := svc.Stats(w.ctx, w.monitor, "", Range24h, now, true)
	if err != nil {
		t.Fatal(err)
	}
	liveNode, err := svc.Stats(w.ctx, w.monitor, "n1", Range24h, now, true)
	if err != nil {
		t.Fatal(err)
	}

	runUntilIdle(t, w.job(metrics, &now, DefaultRetention), metrics)
	if wm, _ := metrics.Watermark(w.ctx, wmHour); wm != at(time.Hour) {
		t.Fatalf("hour watermark = %v, want +1h", time.UnixMilli(wm).UTC())
	}

	// The outage hour, consensus and per node.
	c := rollup(t, metrics, "", hourMs, at(-10*time.Hour))
	if c.UpMs != 30*minute || c.DownMs != 30*minute || c.UnknownMs != 0 || c.LatencyCount != 30 || c.LatencyAvg != 100 {
		t.Fatalf("consensus hour = %+v", c)
	}
	n := rollup(t, metrics, "n1", hourMs, at(-10*time.Hour))
	if n.UpMs != 30*minute || n.DownMs != 30*minute || n.LatencyCount != 30 {
		t.Fatalf("node hour = %+v", n)
	}
	// n2 started at -2h; in its first hour the consensus latency mixes both nodes.
	c2 := rollup(t, metrics, "", hourMs, at(-2*time.Hour))
	if c2.LatencyCount != 120 || c2.LatencyAvg != 75 {
		t.Fatalf("mixed latency = %+v", c2)
	}
	// The day before t0 is the sum of its hours.
	d := rollup(t, metrics, "", dayMs, at(-24*time.Hour))
	if d.UpMs != 24*hour-30*minute || d.DownMs != 30*minute || d.UnknownMs != 0 {
		t.Fatalf("day = %+v", d)
	}

	// Rolled-up buckets equal what was computed live from the raw data.
	rolled, err := svc.Stats(w.ctx, w.monitor, "", Range24h, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(live, rolled) {
		t.Fatalf("live and rolled-up stats differ:\nlive   %+v\nrolled %+v", live.Total, rolled.Total)
	}
	rolledNode, err := svc.Stats(w.ctx, w.monitor, "n1", Range24h, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(liveNode, rolledNode) {
		t.Fatalf("node stats differ:\nlive   %+v\nrolled %+v", liveNode.Total, rolledNode.Total)
	}
	if len(rolled.Buckets) != 24 || rolled.Buckets[23].StartMs != at(time.Hour) || rolled.Buckets[23].UpMs != 5*minute {
		t.Fatalf("buckets = %d, last = %+v", len(rolled.Buckets), rolled.Buckets[len(rolled.Buckets)-1])
	}
	// 23 full hours plus the 5 minutes of the current hour; 30 minutes down.
	if pct, _ := Percent(rolled.Total); pct != 100*float64(23*hour+5*minute-30*minute)/float64(23*hour+5*minute) {
		t.Fatalf("24h uptime = %v", pct)
	}

	// Ranges with daily buckets use the day rollups and sum today's hours.
	month, err := svc.Stats(w.ctx, w.monitor, "", Range30d, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(month.Buckets) != 30 || month.Total.DownMs != 30*minute || month.Buckets[29].UpMs != 65*minute {
		t.Fatalf("30d = %+v, last bucket %+v", month.Total, month.Buckets[29])
	}
}

// TestCompactionKeepsUptime covers FA-UPT-04: uptime before and after
// compaction is identical, status changes stay exact.
func TestCompactionKeepsUptime(t *testing.T) {
	w := newWorld(t)
	metrics := w.newMetrics()
	svc := &Service{State: w.state, Metrics: metrics}
	now := t0.Add(65 * time.Minute)
	j := w.job(metrics, &now, DefaultRetention)
	runUntilIdle(t, j, metrics)

	// Two hours later, [-47h, -45h) is older than 48 h and gets compacted.
	now = t0.Add(3*time.Hour + 5*time.Minute)
	before, err := svc.Stats(w.ctx, w.monitor, "n1", Range7d, now, true)
	if err != nil {
		t.Fatal(err)
	}
	runUntilIdle(t, j, metrics)
	if wm, _ := metrics.Watermark(w.ctx, wmCompaction); wm != at(-45*time.Hour) {
		t.Fatalf("compaction watermark = %v", time.UnixMilli(wm).UTC())
	}
	after, err := svc.Stats(w.ctx, w.monitor, "n1", Range7d, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Total, after.Total) {
		t.Fatalf("uptime changed by compaction:\nbefore %+v\nafter  %+v", before.Total, after.Total)
	}

	page, err := metrics.ListHeartbeats(w.ctx, "t", store.HeartbeatQuery{MonitorID: "m1", FromMs: at(-47 * time.Hour), ToMs: at(-45*time.Hour) - 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].TsMs != at(-2790*time.Minute) || page.Items[0].Status != store.CheckDown ||
		page.Items[1].TsMs != at(-2788*time.Minute) || page.Items[1].Status != store.CheckUp {
		t.Fatalf("kept heartbeats = %+v", page.Items)
	}
	// Uncompacted hours keep every heartbeat.
	if page, _ := metrics.ListHeartbeats(w.ctx, "t", store.HeartbeatQuery{MonitorID: "m1", FromMs: at(-45 * time.Hour), ToMs: at(-44*time.Hour) - 1}); len(page.Items) != 60 {
		t.Fatalf("uncompacted hour has %d heartbeats", len(page.Items))
	}
}

func TestRetention(t *testing.T) {
	w := newWorld(t)
	metrics := w.newMetrics()
	now := t0.Add(65 * time.Minute)
	runUntilIdle(t, w.job(metrics, &now, DefaultRetention), metrics)

	short := w.job(metrics, &now, Retention{Raw: 24 * time.Hour, Hourly: 30 * time.Hour})
	if err := short.RunOnce(w.ctx); err != nil {
		t.Fatal(err)
	}
	cut := now.Add(-24 * time.Hour).UnixMilli()
	if page, _ := metrics.ListHeartbeats(w.ctx, "t", store.HeartbeatQuery{MonitorID: "m1", ToMs: cut - 1}); len(page.Items) != 0 {
		t.Fatalf("%d heartbeats older than the raw retention remain", len(page.Items))
	}
	if page, _ := metrics.ListHeartbeats(w.ctx, "t", store.HeartbeatQuery{MonitorID: "m1", FromMs: cut}); len(page.Items) == 0 {
		t.Fatal("recent heartbeats deleted")
	}
	hours, _ := metrics.ListRollups(w.ctx, "t", store.RollupQuery{MonitorID: "m1", AllNodes: true, BucketMs: hourMs, ToMs: now.Add(-30 * time.Hour).UnixMilli()})
	days, _ := metrics.ListRollups(w.ctx, "t", store.RollupQuery{MonitorID: "m1", AllNodes: true, BucketMs: dayMs, ToMs: at(0)})
	if len(hours) != 0 || len(days) == 0 {
		t.Fatalf("hourly rollups past retention = %d, daily rollups = %d", len(hours), len(days))
	}
}

// TestDeterministicRollups covers FA-UPT-03: two voters with the same data
// compute identical rollups.
func TestDeterministicRollups(t *testing.T) {
	w := newWorld(t)
	now := t0.Add(65 * time.Minute)
	var all [][]store.Rollup
	for range 2 {
		metrics := w.newMetrics()
		runUntilIdle(t, w.job(metrics, &now, DefaultRetention), metrics)
		var rs []store.Rollup
		for _, b := range []int64{hourMs, dayMs} {
			part, err := metrics.ListRollups(w.ctx, "t", store.RollupQuery{MonitorID: "m1", AllNodes: true, BucketMs: b, ToMs: at(48 * time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			rs = append(rs, part...)
		}
		all = append(all, rs)
	}
	if len(all[0]) == 0 || !reflect.DeepEqual(all[0], all[1]) {
		t.Fatalf("rollups differ: %d vs %d rows", len(all[0]), len(all[1]))
	}
}
