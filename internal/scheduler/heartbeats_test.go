// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
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

// The OnStored hook receives every stored batch, and only stored ones.
func TestHeartbeatWriterOnStored(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics, err := sqlite.OpenMetrics(ctx, t.TempDir(), sqlite.MetricsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = metrics.Close() }()

	w, err := NewHeartbeatWriter(ctx, metrics, "node-1", log)
	if err != nil {
		t.Fatal(err)
	}
	var stored []store.Heartbeat
	w.SetOnStored(func(hbs []store.Heartbeat) { stored = append(stored, hbs...) })

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { w.Run(runCtx); close(done) }()
	m := store.Monitor{TenantID: "t", ID: "m"}
	for i := range 4 {
		w.Record(runCtx, Outcome{Monitor: m, Started: time.UnixMilli(int64(1000 * (i + 1))),
			Result: checker.Result{Status: checker.StatusUp, LatencyMs: 3}})
	}
	cancel()
	<-done

	if len(stored) != 4 {
		t.Fatalf("hook received %d heartbeats, want 4", len(stored))
	}
	for i, hb := range stored {
		if hb.Seq != uint64(i+1) {
			t.Errorf("hook heartbeat %d has seq %d", i, hb.Seq)
		}
	}
}

// insertFailer fails the first failures calls of InsertHeartbeats and delegates
// the rest to the real store.
type insertFailer struct {
	store.Metrics
	failures int
}

func (m *insertFailer) InsertHeartbeats(ctx context.Context, hbs []store.Heartbeat) (int, error) {
	if m.failures > 0 {
		m.failures--
		return 0, errors.New("disk full")
	}
	return m.Metrics.InsertHeartbeats(ctx, hbs)
}

// The OnStored hook is not called while the insert fails, and it receives the
// batch exactly once after the retry stored it (ADR 0021).
func TestHeartbeatWriterOnStoredInsertErrors(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	realMetrics, err := sqlite.OpenMetrics(context.Background(), t.TempDir(), sqlite.MetricsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = realMetrics.Close() }()
	batch := []store.Heartbeat{
		{NodeID: "node-1", Incarnation: realMetrics.Incarnation(), Seq: 1, TenantID: "t", MonitorID: "m", TsMs: 1000, Details: []byte("{}")},
		{NodeID: "node-1", Incarnation: realMetrics.Incarnation(), Seq: 2, TenantID: "t", MonitorID: "m", TsMs: 2000, Details: []byte("{}")},
	}

	t.Run("no call while the insert fails", func(t *testing.T) {
		metrics := &insertFailer{Metrics: realMetrics, failures: 1 << 30}
		w, err := NewHeartbeatWriter(context.Background(), metrics, "node-1", log)
		if err != nil {
			t.Fatal(err)
		}
		var calls int
		w.SetOnStored(func([]store.Heartbeat) { calls++ })
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // flush gives up on the retry as soon as ctx ends
		w.flush(ctx, batch)
		if calls != 0 {
			t.Fatalf("hook called %d times for a failed insert, want 0", calls)
		}
	})

	t.Run("called once with the stored batch after a retry", func(t *testing.T) {
		metrics := &insertFailer{Metrics: realMetrics, failures: 1}
		w, err := NewHeartbeatWriter(context.Background(), metrics, "node-1", log)
		if err != nil {
			t.Fatal(err)
		}
		var got [][]store.Heartbeat
		w.SetOnStored(func(hbs []store.Heartbeat) { got = append(got, slices.Clone(hbs)) })
		w.flush(context.Background(), batch)
		if len(got) != 1 || len(got[0]) != 2 || got[0][0].Seq != 1 || got[0][1].Seq != 2 {
			t.Fatalf("hook calls = %+v, want one call with seqs 1 and 2", got)
		}
	})
}
