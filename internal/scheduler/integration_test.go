// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package scheduler_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/aggregator"
	"github.com/joinsider/pulsequorum/internal/checker"
	_ "github.com/joinsider/pulsequorum/internal/checker/builtin"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/scheduler"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

// TestEndToEnd runs the phase-1 pipeline: a monitor created through Raft is
// picked up by the reconciler, checked by the scheduler and stored as a
// heartbeat, and the aggregator records the consensus status UP.
func TestEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer target.Close()

	state, err := sqlite.OpenState(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = state.Close() }()
	metrics, err := sqlite.OpenMetrics(ctx, dir, sqlite.MetricsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = metrics.Close() }()

	node, err := cluster.Start(ctx, cluster.Config{NodeID: "44444444-4444-4444-4444-444444444444", Advertise: "n:9443",
		DataDir: dir, Timeout: 200 * time.Millisecond, Logger: log, RaftLogLevel: "error"}, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = node.Shutdown() }()
	if err := cluster.EnsureDefaultTenant(ctx, node, state); err != nil {
		t.Fatal(err)
	}

	writer, err := scheduler.NewHeartbeatWriter(ctx, metrics, "node-1", log)
	if err != nil {
		t.Fatal(err)
	}
	agg := aggregator.New(aggregator.Options{State: state, Metrics: metrics, Cluster: node, NodeID: "node-1",
		IsLeader: node.IsLeader, LeaderGrace: time.Millisecond, Unit: 10 * time.Millisecond, Logger: log})
	writer.SetOnStored(agg.Observe)
	sched := scheduler.New(scheduler.Options{Registry: checker.Default, Env: &checker.Env{Dialer: &checker.Dialer{}, UserAgent: "test"},
		Sink: writer, Logger: log, Unit: 10 * time.Millisecond})
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{}, 4)
	go func() { writer.Run(runCtx); done <- struct{}{} }()
	go func() { agg.Run(runCtx); done <- struct{}{} }()
	go func() { sched.Run(runCtx); done <- struct{}{} }()
	go func() {
		scheduler.Reconcile(runCtx, state, node.FSM().Changes(), time.Hour, log, sched, agg)
		done <- struct{}{}
	}()

	cmd, err := cluster.NewPutMonitor("create-1", cluster.DefaultTenantID, "mon-1", cluster.MonitorDocument{
		Name: "target", Type: "http", Config: json.RawMessage(`{"url":"` + target.URL + `"}`),
		IntervalSeconds: 20, TimeoutSeconds: 10, Assignment: store.Assignment{Mode: "all"}, ConsensusPolicy: "majority",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.Apply(ctx, cmd); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		page, err := metrics.ListHeartbeats(ctx, cluster.DefaultTenantID, store.HeartbeatQuery{MonitorID: "mon-1"})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > 0 {
			if hb := page.Items[0]; hb.Status != store.CheckUp || hb.Seq != 1 {
				t.Fatalf("heartbeat = %+v", hb)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no heartbeat stored")
		}
		time.Sleep(50 * time.Millisecond)
	}
	for {
		m, err := state.GetMonitor(ctx, cluster.DefaultTenantID, "mon-1")
		if err != nil {
			t.Fatal(err)
		}
		if m.State.Status == store.StatusUp {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %s, want UP", m.State.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	for range 4 {
		<-done
	}
}
