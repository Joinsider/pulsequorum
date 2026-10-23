// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/checker"
	"github.com/joinsider/uptimecluster/internal/checker/icmpcheck"
	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite"
)

func TestReconcile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state, err := sqlite.OpenState(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = state.Close() }()
	put := func(fn func(tx store.StateTx) error) {
		t.Helper()
		if err := state.Update(ctx, fn); err != nil {
			t.Fatal(err)
		}
	}
	put(func(tx store.StateTx) error {
		for _, id := range []string{"t1", "t2"} {
			if err := tx.PutTenant(ctx, store.Tenant{ID: id, Name: id, Plan: "default"}); err != nil {
				return err
			}
		}
		return nil
	})
	addMonitor := func(tenant string, i int) {
		put(func(tx store.StateTx) error {
			return tx.PutMonitor(ctx, store.Monitor{TenantID: tenant, ID: fmt.Sprintf("%s-m%03d", tenant, i), Name: "m", Type: "fake",
				IntervalSeconds: 3600, TimeoutSeconds: 10, Assignment: store.Assignment{Mode: "all"}, ConsensusPolicy: "majority"}, 0)
		})
	}
	// More than one page for t1 to cover paging.
	for i := range 501 {
		addMonitor("t1", i)
	}
	addMonitor("t2", 0)

	s, _, _ := newScheduler(t, 0)
	changes := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		Reconcile(ctx, state, changes, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), s)
		close(done)
	}()
	eventually(t, func() bool { return s.Len() == 502 }, "initial load incomplete")

	addMonitor("t2", 1)
	changes <- struct{}{}
	eventually(t, func() bool { return s.Len() == 503 }, "change signal not applied")

	cancel()
	<-done
}

// syncRecorder keeps the last set of monitors it was given.
type syncRecorder struct {
	calls    int
	monitors []store.Monitor
}

func (r *syncRecorder) Sync(monitors []store.Monitor) {
	r.calls++
	r.monitors = monitors
}

// TestAssignmentSyncer covers FA-SCH-01/02: a node runs the monitors assigned
// to it and keeps its assignment while the registry cannot be read.
func TestAssignmentSyncer(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	monitors := []store.Monitor{
		{ID: "all", Type: "fake", Assignment: store.Assignment{Mode: "all"}},
		{ID: "pinned", Type: "fake", Assignment: store.Assignment{Mode: "nodes", Nodes: []string{"n2"}}},
		{ID: "icmp", Type: "icmp", Assignment: store.Assignment{Mode: "all"}},
	}
	registry := []store.Node{
		voter("n1", true, "icmp"),
		voter("n2", true),
	}
	ids := func(ms []store.Monitor) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return out
	}

	t.Run("node with the capability runs what is assigned to it", func(t *testing.T) {
		target := &syncRecorder{}
		s := AssignmentSyncer{Target: target, NodeID: "n1", Assigner: Assigner{Capability: Capabilities(checker.Default)},
			Registry: func() ([]store.Node, error) { return registry, nil }, Log: log}
		s.Sync(monitors)
		if got, want := ids(target.monitors), []string{"all", "icmp"}; !slices.Equal(got, want) {
			t.Errorf("monitors = %v, want %v", got, want)
		}
	})
	t.Run("node without the capability skips the icmp monitor", func(t *testing.T) {
		target := &syncRecorder{}
		s := AssignmentSyncer{Target: target, NodeID: "n2", Assigner: Assigner{Capability: Capabilities(checker.Default)},
			Registry: func() ([]store.Node, error) { return registry, nil }, Log: log}
		s.Sync(monitors)
		if got, want := ids(target.monitors), []string{"all", "pinned"}; !slices.Equal(got, want) {
			t.Errorf("monitors = %v, want %v", got, want)
		}
	})
	t.Run("empty registry runs everything", func(t *testing.T) {
		target := &syncRecorder{}
		s := AssignmentSyncer{Target: target, NodeID: "n1", Assigner: Assigner{Capability: Capabilities(checker.Default)},
			Registry: func() ([]store.Node, error) { return nil, nil }, Log: log}
		s.Sync(monitors)
		if len(target.monitors) != len(monitors) {
			t.Errorf("monitors = %v, want all", ids(target.monitors))
		}
	})
	t.Run("registry error keeps the previous assignment", func(t *testing.T) {
		target := &syncRecorder{}
		fail := false
		s := AssignmentSyncer{Target: target, NodeID: "n1", Assigner: Assigner{Capability: Capabilities(checker.Default)},
			Registry: func() ([]store.Node, error) {
				if fail {
					return nil, errors.New("boom")
				}
				return registry, nil
			}, Log: log}
		s.Sync(monitors)
		fail = true
		s.Sync(nil)
		if target.calls != 1 {
			t.Errorf("target synced %d times, want once", target.calls)
		}
	})
}

func TestCapabilities(t *testing.T) {
	capability := Capabilities(checker.Default)
	tests := []struct {
		monitorType string
		want        string
	}{
		{"icmp", icmpcheck.Capability},
		{"http", ""},
		{"unknown-type", ""},
	}
	for _, tt := range tests {
		if got := capability(tt.monitorType); got != tt.want {
			t.Errorf("Capabilities(%q) = %q, want %q", tt.monitorType, got, tt.want)
		}
	}
}
