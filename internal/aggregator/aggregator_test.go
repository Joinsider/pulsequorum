// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package aggregator

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/joinsider/uptimecluster/internal/checker"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/scheduler"
	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite"
)

const nodeID = "66666666-6666-6666-6666-666666666666"

type fixture struct {
	t       *testing.T
	ctx     context.Context
	state   *sqlite.StateStore
	metrics *sqlite.MetricsStore
	node    *cluster.Node
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	dir := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	state, err := sqlite.OpenState(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := sqlite.OpenMetrics(ctx, dir, sqlite.MetricsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	node, err := cluster.Start(ctx, cluster.Config{NodeID: nodeID, Advertise: "n:9443", DataDir: dir,
		Timeout: 200 * time.Millisecond, Logger: log, RaftLogLevel: "error"}, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = metrics.Close()
		_ = state.Close()
	})
	if err := cluster.EnsureDefaultTenant(ctx, node, state); err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, ctx: ctx, state: state, metrics: metrics, node: node}
}

func (f *fixture) aggregator() *Aggregator {
	return New(Options{State: f.state, Metrics: f.metrics, Cluster: f.node, NodeID: nodeID, Logger: slog.New(slog.DiscardHandler)})
}

func (f *fixture) putMonitor(doc cluster.MonitorDocument, rev uint64) store.Monitor {
	f.t.Helper()
	cmd, err := cluster.NewPutMonitor("", cluster.DefaultTenantID, "m1", doc, rev)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.node.Apply(f.ctx, cmd); err != nil {
		f.t.Fatal(err)
	}
	return f.monitor()
}

func (f *fixture) monitor() store.Monitor {
	f.t.Helper()
	m, err := f.state.GetMonitor(f.ctx, cluster.DefaultTenantID, "m1")
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

func (f *fixture) expect(status store.MonitorStatus, sinceMs int64) {
	f.t.Helper()
	if st := f.monitor().State; st.Status != status || st.SinceMs != sinceMs {
		f.t.Fatalf("state = %s since %d, want %s since %d", st.Status, st.SinceMs, status, sinceMs)
	}
}

func outcome(m store.Monitor, atMs int64, status checker.Status) scheduler.Outcome {
	r := checker.Result{Status: status, LatencyMs: 5}
	if status == checker.StatusDown {
		r.Message, r.ErrorClass = "connection refused", checker.ClassConnect
	}
	return scheduler.Outcome{Monitor: m, Started: time.UnixMilli(atMs), Scheduled: time.UnixMilli(atMs), Result: r}
}

func doc(retries, recovery int, paused bool) cluster.MonitorDocument {
	return cluster.MonitorDocument{Name: "web", Type: "http", IntervalSeconds: 60, TimeoutSeconds: 10, Retries: retries,
		RecoveryThreshold: recovery, Assignment: store.Assignment{Mode: "all"}, ConsensusPolicy: "majority", Paused: paused}
}

// TestIncidentLifecycle drives a monitor through DOWN and back and checks the
// replicated state and incident (FA-CON-03, FA-INC-01).
func TestIncidentLifecycle(t *testing.T) {
	f := newFixture(t)
	a := f.aggregator()
	m := f.putMonitor(doc(1, 2, false), 0)

	a.process(f.ctx, outcome(m, 10_000, checker.StatusDown))
	f.expect(store.StatusPending, m.CreatedMs) // still the initial PENDING
	a.process(f.ctx, outcome(m, 70_000, checker.StatusDown))
	f.expect(store.StatusDown, 10_000)

	page, err := f.state.ListIncidents(f.ctx, cluster.DefaultTenantID, store.IncidentQuery{OpenOnly: true})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("open incidents = %+v, %v", page.Items, err)
	}
	inc := page.Items[0]
	if inc.StartedMs != 10_000 || inc.Cause != "connection refused" || inc.ErrorClass != "connect" ||
		len(inc.AffectedNodes) != 1 || inc.AffectedNodes[0] != nodeID {
		t.Fatalf("incident = %+v", inc)
	}

	a.process(f.ctx, outcome(m, 130_000, checker.StatusUp))
	f.expect(store.StatusDown, 10_000) // recovery threshold 2
	a.process(f.ctx, outcome(m, 190_000, checker.StatusUp))
	f.expect(store.StatusUp, 130_000)
	got, err := f.state.GetIncident(f.ctx, cluster.DefaultTenantID, inc.ID)
	if err != nil || got.ResolvedMs != 130_000 {
		t.Fatalf("resolved incident = %+v, %v", got, err)
	}

	// Pausing ends the evaluation: a stale result cannot leave PAUSED.
	paused := f.putMonitor(doc(1, 2, true), m.Revision)
	a.process(f.ctx, outcome(m, 250_000, checker.StatusDown))
	a.process(f.ctx, outcome(m, 310_000, checker.StatusDown))
	if st := f.monitor().State.Status; st != store.StatusPaused {
		t.Fatalf("state after pause = %s", st)
	}
	resumed := f.putMonitor(doc(1, 2, false), paused.Revision)
	f.expect(store.StatusPending, resumed.UpdatedMs)
	a.process(f.ctx, outcome(resumed, resumed.UpdatedMs+1000, checker.StatusUp))
	f.expect(store.StatusUp, resumed.UpdatedMs+1000)
}

// TestRetryCountSurvivesRestart checks that a new aggregator rebuilds the
// failure streak from this node's heartbeats (NFA-REL-03).
func TestRetryCountSurvivesRestart(t *testing.T) {
	f := newFixture(t)
	m := f.putMonitor(doc(2, 1, false), 0)
	a := f.aggregator()
	a.process(f.ctx, outcome(m, 10_000, checker.StatusUp))
	f.expect(store.StatusUp, 10_000)

	// Two failures were recorded before the restart; one by another node
	// and one older than the last status change must not count.
	hbs := []store.Heartbeat{
		{NodeID: nodeID, Incarnation: f.metrics.Incarnation(), Seq: 1, TenantID: cluster.DefaultTenantID, MonitorID: "m1", TsMs: 5_000, Status: store.CheckDown},
		{NodeID: nodeID, Incarnation: f.metrics.Incarnation(), Seq: 2, TenantID: cluster.DefaultTenantID, MonitorID: "m1", TsMs: 10_000, Status: store.CheckUp},
		{NodeID: nodeID, Incarnation: f.metrics.Incarnation(), Seq: 3, TenantID: cluster.DefaultTenantID, MonitorID: "m1", TsMs: 70_000, Status: store.CheckDown, Message: "timeout", ErrorClass: "timeout"},
		{NodeID: "other", Incarnation: "x", Seq: 1, TenantID: cluster.DefaultTenantID, MonitorID: "m1", TsMs: 100_000, Status: store.CheckDown},
		{NodeID: nodeID, Incarnation: f.metrics.Incarnation(), Seq: 4, TenantID: cluster.DefaultTenantID, MonitorID: "m1", TsMs: 130_000, Status: store.CheckDown, Message: "timeout", ErrorClass: "timeout"},
	}
	if _, err := f.metrics.InsertHeartbeats(f.ctx, hbs); err != nil {
		t.Fatal(err)
	}
	// The state is still UP: the failures above happened while no aggregator ran.
	restarted := f.aggregator()
	restarted.process(f.ctx, outcome(m, 190_000, checker.StatusDown))
	f.expect(store.StatusDown, 70_000)
}

func TestRunAndSync(t *testing.T) {
	f := newFixture(t)
	// Only the aggregator's own goroutine is checked; the fixture stops in Cleanup.
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	m := f.putMonitor(doc(0, 1, false), 0)
	a := f.aggregator()
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()

	a.Record(ctx, outcome(m, 10_000, checker.StatusDown))
	deadline := time.Now().Add(5 * time.Second)
	for f.monitor().State.Status != store.StatusDown {
		if time.Now().After(deadline) {
			t.Fatal("status not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.Sync(nil)
	a.mu.Lock()
	n := len(a.trackers)
	a.mu.Unlock()
	if n != 0 {
		t.Fatalf("trackers after sync = %d, want 0", n)
	}
	cancel()
	<-done
}
