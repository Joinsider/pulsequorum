// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/uptime"
)

// seedListFixture creates n monitors of the default tenant with a mix of
// states and heartbeats for the 26 hours before now, and runs the uptime job
// until its hourly rollups cover the window. Monitors are written to the store
// directly: the cluster stamps creation with the wall clock, which would leave
// the whole window before the creation time unknown.
//
// The mix: every sixth monitor has no heartbeats, every fourth was created
// three hours ago, every fifth had a DOWN period, every seventh is paused,
// every ninth is in maintenance since two hours ago, every third reports from a
// second node, and every eighth stopped reporting two hours ago.
func seedListFixture(tb testing.TB, e *env, n int, now time.Time) []store.Monitor {
	tb.Helper()
	ctx := context.Background()
	const tenant = "default"
	windowStart := now.Add(-26 * time.Hour).UnixMilli()
	nowMs := now.UnixMilli()

	monitors := make([]store.Monitor, 0, n)
	for i := range n {
		created := now.Add(-30 * time.Hour).UnixMilli()
		if i%4 == 3 {
			created = now.Add(-3 * time.Hour).UnixMilli()
		}
		m := store.Monitor{
			TenantID: tenant, ID: uuid.Must(uuid.NewV7()).String(), Name: fmt.Sprintf("svc-%04d", i),
			Type: "http", Config: json.RawMessage(fmt.Sprintf(`{"url":"https://example.com/svc-%d"}`, i)),
			IntervalSeconds: 60, TimeoutSeconds: 24, Retries: 1, RecoveryThreshold: 1, ResendIntervalMinutes: 60,
			Assignment: store.Assignment{Mode: "all"}, ConsensusPolicy: "majority",
			Paused:    i%7 == 2,
			CreatedMs: created, UpdatedMs: created,
		}
		monitors = append(monitors, m)
	}

	// Status history: a change at creation, then the scenario's changes.
	err := e.state.Update(ctx, func(tx store.StateTx) error {
		for i, m := range monitors {
			if err := tx.PutMonitor(ctx, m, 0); err != nil {
				return err
			}
			if err := tx.PutMonitorState(ctx, tenant, m.ID, store.MonitorState{Status: store.StatusUp, Confirmed: true, SinceMs: m.CreatedMs}); err != nil {
				return err
			}
			if i%5 == 0 {
				down := now.Add(-4 * time.Hour).UnixMilli()
				if err := tx.PutMonitorState(ctx, tenant, m.ID, store.MonitorState{Status: store.StatusDown, Confirmed: true, SinceMs: down}); err != nil {
					return err
				}
				if err := tx.PutMonitorState(ctx, tenant, m.ID, store.MonitorState{Status: store.StatusUp, Confirmed: true, SinceMs: down + int64(time.Hour/time.Millisecond)}); err != nil {
					return err
				}
			}
			if i%9 == 4 {
				if err := tx.PutMonitorState(ctx, tenant, m.ID, store.MonitorState{Status: store.StatusMaintenance, Confirmed: true, SinceMs: now.Add(-2 * time.Hour).UnixMilli()}); err != nil {
					return err
				}
			}
			if m.Paused {
				if err := tx.PutMonitorState(ctx, tenant, m.ID, store.MonitorState{Status: store.StatusPaused, Confirmed: true, SinceMs: now.Add(-time.Hour).UnixMilli()}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		tb.Fatalf("seed monitors: %v", err)
	}

	var batch []store.Heartbeat
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if _, err := e.metrics.InsertHeartbeats(ctx, batch); err != nil {
			tb.Fatalf("seed heartbeats: %v", err)
		}
		batch = batch[:0]
	}
	for i, m := range monitors {
		if i%6 == 5 {
			continue
		}
		start := max(m.CreatedMs, windowStart)
		end := nowMs
		if i%8 == 7 {
			end = now.Add(-2 * time.Hour).UnixMilli()
		}
		nodes := []string{"n1"}
		if i%3 == 1 {
			nodes = append(nodes, "n2")
		}
		for ni, node := range nodes {
			seq := uint64(0)
			for ts := start + int64(ni)*20_000; ts <= end; ts += 60_000 {
				seq++
				status := store.CheckUp
				if i%5 == 0 && ts >= now.Add(-4*time.Hour).UnixMilli() && ts < now.Add(-3*time.Hour).UnixMilli() {
					status = store.CheckDown
				}
				batch = append(batch, store.Heartbeat{
					NodeID: node, Incarnation: "inc-" + node, Seq: seq, TenantID: tenant, MonitorID: m.ID,
					TsMs: ts, Status: status, LatencyMs: 10 + int((ts/60_000+int64(i))%37),
				})
				if len(batch) >= 20_000 {
					flush()
				}
			}
		}
	}
	flush()

	job := uptime.NewJob(uptime.JobOptions{State: e.state, Metrics: e.metrics, Now: func() time.Time { return now }})
	// One run rolls up at most 24 hours; the window needs two or three.
	for range 3 {
		if err := job.RunOnce(ctx); err != nil {
			tb.Fatalf("uptime job: %v", err)
		}
	}
	return monitors
}

// BenchmarkListMonitors measures GET /api/v1/monitors?limit=500 for 500
// monitors with heartbeats and rollups (NFA-PERF-03).
func BenchmarkListMonitors(b *testing.B) {
	e := newEnvWith(b, false)
	now := time.Now().Truncate(time.Minute)
	e.now = now
	seedListFixture(b, e, 500, now)

	b.ResetTimer()
	for range b.N {
		res := e.do("GET", "/api/v1/monitors?limit=500", viewer, nil)
		if res.code != http.StatusOK {
			b.Fatalf("list = %d %s", res.code, res.body)
		}
	}
}

// listedSummary is what the list returns per monitor beyond the configuration.
type listedSummary struct {
	lastCheckAt   *time.Time
	lastLatencyMs *int
	uptime24h     *float64
}

// referenceSummary is the per-monitor computation that ListMonitors used
// before it was batched: one latest-heartbeat query and one 24 h Stats call
// for each monitor. The list must return exactly these values.
func referenceSummary(t *testing.T, e *env, m store.Monitor, now time.Time) listedSummary {
	t.Helper()
	ctx := context.Background()
	var out listedSummary
	last, err := e.metrics.LatestHeartbeats(ctx, m.TenantID, m.ID, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(last) > 0 {
		at := time.UnixMilli(last[0].TsMs).UTC()
		lat := last[0].LatencyMs
		out.lastCheckAt, out.lastLatencyMs = &at, &lat
	}
	svc := &uptime.Service{State: e.state, Metrics: e.metrics}
	stats, err := svc.Stats(ctx, m, "", uptime.Range24h, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if pct, ok := uptime.Percent(stats.Total); ok {
		out.uptime24h = &pct
	}
	return out
}

// TestListMonitorsMatchesPerMonitorValues checks that the batched list returns
// the same heartbeat and uptime fields as the per-monitor computation for a mix
// of monitors: with and without heartbeats, paused, in maintenance, created
// inside the window, and stale.
func TestListMonitorsMatchesPerMonitorValues(t *testing.T) {
	e := newEnv(t)
	now := time.Now().Truncate(time.Minute)
	e.now = now
	monitors := seedListFixture(t, e, 60, now)

	page := decode[gen.MonitorPage](t, e.do("GET", "/api/v1/monitors?limit=500", viewer, nil))
	if len(page.Items) != len(monitors) {
		t.Fatalf("listed %d monitors, want %d", len(page.Items), len(monitors))
	}
	byID := map[string]store.Monitor{}
	for _, m := range monitors {
		byID[m.ID] = m
	}
	var withLast, withUptime, withoutLast int
	for _, item := range page.Items {
		m, ok := byID[item.Id.String()]
		if !ok {
			t.Fatalf("unexpected monitor %s", item.Id)
		}
		want := referenceSummary(t, e, m, now)
		got := listedSummary{lastCheckAt: item.LastCheckAt, lastLatencyMs: item.LastLatencyMs, uptime24h: item.Uptime24h}
		if !sameSummary(got, want) {
			t.Errorf("monitor %s: list = %s, per-monitor = %s", m.Name, describe(got), describe(want))
		}
		if want.lastCheckAt != nil {
			withLast++
		} else {
			withoutLast++
		}
		if want.uptime24h != nil {
			withUptime++
		}
	}
	// The fixture must cover both cases, or the comparison proves little.
	if withLast == 0 || withoutLast == 0 || withUptime == 0 {
		t.Fatalf("fixture lacks variety: with heartbeats %d, without %d, with uptime %d", withLast, withoutLast, withUptime)
	}
}

func sameSummary(a, b listedSummary) bool {
	if (a.lastCheckAt == nil) != (b.lastCheckAt == nil) || (a.lastLatencyMs == nil) != (b.lastLatencyMs == nil) ||
		(a.uptime24h == nil) != (b.uptime24h == nil) {
		return false
	}
	if a.lastCheckAt != nil && !a.lastCheckAt.Equal(*b.lastCheckAt) {
		return false
	}
	if a.lastLatencyMs != nil && *a.lastLatencyMs != *b.lastLatencyMs {
		return false
	}
	return a.uptime24h == nil || *a.uptime24h == *b.uptime24h
}

func describe(s listedSummary) string {
	out := "{"
	if s.lastCheckAt != nil {
		out += fmt.Sprintf("lastCheckAt=%s ", s.lastCheckAt.Format(time.RFC3339Nano))
	}
	if s.lastLatencyMs != nil {
		out += fmt.Sprintf("lastLatencyMs=%d ", *s.lastLatencyMs)
	}
	if s.uptime24h != nil {
		out += fmt.Sprintf("uptime24h=%v ", *s.uptime24h)
	}
	return out + "}"
}
