// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package uptime

import (
	"testing"

	"github.com/joinsider/uptimecluster/internal/store"
)

func hb(ts int64, status store.CheckStatus) store.Heartbeat {
	return store.Heartbeat{TsMs: ts, Status: status, LatencyMs: int(ts / 1000)}
}

const (
	up   = store.CheckUp
	down = store.CheckDown
	deg  = store.CheckDegraded
)

// TestNodeUpDown covers the attribution of one node's stream (FA-UPT-01/02,
// FA-CON-05): a heartbeat counts until the next one, at most two intervals.
func TestNodeUpDown(t *testing.T) {
	const cover = 120 // interval 60
	tests := []struct {
		name         string
		hbs          []store.Heartbeat
		start, end   int64
		wantUp, wDwn int64
	}{
		{"regular", []store.Heartbeat{hb(0, up), hb(60, down), hb(120, up)}, 0, 180, 120, 60},
		{"degraded counts as up", []store.Heartbeat{hb(0, deg), hb(60, up)}, 0, 120, 120, 0},
		{"gap beyond two intervals is unknown", []store.Heartbeat{hb(0, up), hb(300, up)}, 0, 360, 180, 0},
		{"last heartbeat expires", []store.Heartbeat{hb(0, down)}, 0, 1000, 0, 120},
		{"lookback heartbeat covers the start", []store.Heartbeat{hb(-30, down), hb(30, up)}, 0, 60, 30, 30},
		{"expired lookback heartbeat", []store.Heartbeat{hb(-200, down), hb(30, up)}, 0, 60, 30, 0},
		{"clipped at end", []store.Heartbeat{hb(50, up)}, 0, 100, 50, 0},
		{"no heartbeats", nil, 0, 100, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, d := nodeUpDown(tt.hbs, tt.start, tt.end, cover)
			if u != tt.wantUp || d != tt.wDwn {
				t.Fatalf("up/down = %d/%d, want %d/%d", u, d, tt.wantUp, tt.wDwn)
			}
		})
	}
}

func change(since int64, s store.MonitorStatus) store.StatusChange {
	return store.StatusChange{SinceMs: since, Status: s}
}

func TestConsensusTimes(t *testing.T) {
	tests := []struct {
		name              string
		changes           []store.StatusChange
		start, end        int64
		wantUp, wDn, wMnt int64
	}{
		{"before the first change is unknown", []store.StatusChange{change(40, store.StatusUp)}, 0, 100, 60, 0, 0},
		{"change before the bucket defines its start", []store.StatusChange{change(-50, store.StatusDown), change(30, store.StatusUp)}, 0, 100, 70, 30, 0},
		{"pending after up counts as up", []store.StatusChange{change(0, store.StatusUp), change(20, store.StatusPending), change(50, store.StatusDown)}, 0, 100, 50, 50, 0},
		{"initial pending is unknown", []store.StatusChange{change(0, store.StatusPending), change(50, store.StatusUp)}, 0, 100, 50, 0, 0},
		{"pending after down is unknown", []store.StatusChange{change(0, store.StatusDown), change(40, store.StatusPending)}, 0, 100, 0, 40, 0},
		{"paused and unknown are unknown", []store.StatusChange{change(0, store.StatusPaused), change(30, store.StatusUnknown), change(60, store.StatusPartial)}, 0, 100, 40, 0, 0},
		{"maintenance", []store.StatusChange{change(0, store.StatusMaintenance), change(25, store.StatusDegraded)}, 0, 100, 75, 0, 25},
		{"no history", nil, 0, 100, 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, d, m := consensusTimes(tt.changes, tt.start, tt.end)
			if u != tt.wantUp || d != tt.wDn || m != tt.wMnt {
				t.Fatalf("up/down/maint = %d/%d/%d, want %d/%d/%d", u, d, m, tt.wantUp, tt.wDn, tt.wMnt)
			}
		})
	}
}

func TestLatencyAndMerge(t *testing.T) {
	l := latencyOf([]int{5, 1, 4, 2, 3, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20})
	if l.count != 20 || l.avg != 10.5 || l.p95 != 19 {
		t.Fatalf("latency = %+v", l)
	}
	if (latencyOf(nil) != latencyStats{}) {
		t.Fatal("empty latency must be zero")
	}
	got := merge([]store.Rollup{
		{UpMs: 10, DownMs: 5, UnknownMs: 1, LatencyCount: 1, LatencyAvg: 10, LatencyP95: 10},
		{UpMs: 20, MaintMs: 3, LatencyCount: 3, LatencyAvg: 30, LatencyP95: 50},
		{UnknownMs: 7},
	}, store.Rollup{})
	if got.UpMs != 30 || got.DownMs != 5 || got.MaintMs != 3 || got.UnknownMs != 8 ||
		got.LatencyCount != 4 || got.LatencyAvg != 25 || got.LatencyP95 != 40 {
		t.Fatalf("merge = %+v", got)
	}
	if pct, ok := Percent(store.Rollup{UpMs: 3, DownMs: 1, UnknownMs: 100}); !ok || pct != 75 {
		t.Fatalf("percent = %v %v", pct, ok)
	}
	if _, ok := Percent(store.Rollup{MaintMs: 5, UnknownMs: 5}); ok {
		t.Fatal("percent without known time must be undefined")
	}
}

func TestCompactRows(t *testing.T) {
	m := store.Monitor{TenantID: "t", ID: "m"}
	key := func(seq uint64) store.Heartbeat {
		return store.Heartbeat{NodeID: "n1", Incarnation: "i", Seq: seq}
	}
	rows := []store.Heartbeat{
		withTs(key(1), -30_000, up, 10), // lookback: defines the previous status
		withTs(key(2), 0, up, 20),       // same status → deleted
		withTs(key(3), 30_000, down, 0), // change → kept
		withTs(key(4), 60_000, down, 0), // deleted
		withTs(key(5), 90_000, up, 40),  // change → kept
		withTs(key(6), 100_000, up, 60), // deleted
	}
	minutes, deleted := compactRows(m, map[string][]store.Heartbeat{"n1": rows}, 0)
	var seqs []uint64
	for _, k := range deleted {
		seqs = append(seqs, k.Seq)
	}
	if len(seqs) != 3 || seqs[0] != 2 || seqs[1] != 4 || seqs[2] != 6 {
		t.Fatalf("deleted = %v", seqs)
	}
	if len(minutes) != 2 {
		t.Fatalf("minutes = %+v", minutes)
	}
	m0, m1 := minutes[0], minutes[1]
	if m0.MinuteMs != 0 || m0.OKCount != 1 || m0.ErrCount != 1 || m0.Min != 20 || m0.Max != 20 ||
		m1.MinuteMs != 60_000 || m1.OKCount != 2 || m1.ErrCount != 1 || m1.Min != 40 || m1.Max != 60 || m1.Avg != 50 {
		t.Fatalf("minutes = %+v", minutes)
	}

	// Without a previous heartbeat the first one is kept.
	_, deleted = compactRows(m, map[string][]store.Heartbeat{"n1": rows[1:3]}, 0)
	if len(deleted) != 0 {
		t.Fatalf("deleted without lookback = %+v", deleted)
	}
}

func withTs(h store.Heartbeat, ts int64, s store.CheckStatus, latency int) store.Heartbeat {
	h.TsMs, h.Status, h.LatencyMs = ts, s, latency
	return h
}
