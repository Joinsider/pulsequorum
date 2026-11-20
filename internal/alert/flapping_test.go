// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package alert

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/cluster"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

// fakeApplier records the commands of the watcher and returns err for each.
type fakeApplier struct {
	mu   sync.Mutex
	cmds []*pb.ClearMonitorFlapping
	err  error
}

func (f *fakeApplier) Apply(_ context.Context, cmd *pb.Command) (cluster.ApplyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := cmd.GetClearMonitorFlapping(); c != nil {
		f.cmds = append(f.cmds, c)
	}
	return cluster.ApplyResult{}, f.err
}

func (f *fakeApplier) proposed() []*pb.ClearMonitorFlapping {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*pb.ClearMonitorFlapping(nil), f.cmds...)
}

// flapStore holds monitors with a given state, written directly to state.db.
type flapStore struct {
	t  *testing.T
	st *sqlite.StateStore
}

func newFlapStore(t *testing.T) *flapStore {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.OpenState(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Update(ctx, func(tx store.StateTx) error {
		return tx.PutTenant(ctx, store.Tenant{ID: "t1", Name: "t1", Plan: "default", Limits: json.RawMessage("{}"), CreatedMs: 1, UpdatedMs: 1})
	}); err != nil {
		t.Fatal(err)
	}
	return &flapStore{t: t, st: st}
}

// add creates monitor id with the state; flapping sets the flapping period.
func (f *flapStore) add(id string, state store.MonitorState) {
	f.t.Helper()
	ctx := context.Background()
	m := store.Monitor{TenantID: "t1", ID: id, Name: id, Type: "http", Config: json.RawMessage(`{}`), IntervalSeconds: 60,
		TimeoutSeconds: 10, RecoveryThreshold: 1, Assignment: store.Assignment{Mode: "all"}, ConsensusPolicy: "majority",
		CreatedMs: 1, UpdatedMs: 1}
	if err := f.st.Update(ctx, func(tx store.StateTx) error {
		if err := tx.PutMonitor(ctx, m, 0); err != nil {
			return err
		}
		if err := tx.PutMonitorState(ctx, "t1", id, store.MonitorState{Status: state.Status, Confirmed: true, SinceMs: state.SinceMs}); err != nil {
			return err
		}
		return tx.SetMonitorFlapping(ctx, "t1", id, state.Flapping, state.FlappingSinceMs)
	}); err != nil {
		f.t.Fatal(err)
	}
}

// TestFlappingWatcherCheck covers FA-ALR-06: only monitors quiet for 30 min
// after their flapping period started get a proposal.
func TestFlappingWatcherCheck(t *testing.T) {
	const now = int64(10_000_000)
	quiet := now - flappingQuiet.Milliseconds() // last change exactly 30 min ago
	recent := now - 29*time.Minute.Milliseconds()

	tests := []struct {
		name string
		id   string
		st   store.MonitorState
		want bool
	}{
		{"quiet for 30 min", "quiet", store.MonitorState{Status: store.StatusUp, SinceMs: quiet, Flapping: true, FlappingSinceMs: quiet - 1000}, true},
		{"still changing", "busy", store.MonitorState{Status: store.StatusDown, SinceMs: recent, Flapping: true, FlappingSinceMs: quiet - 1000}, false},
		{"period started recently", "young", store.MonitorState{Status: store.StatusUp, SinceMs: quiet - 5000, Flapping: true, FlappingSinceMs: recent}, false},
		{"not flapping", "calm", store.MonitorState{Status: store.StatusUp, SinceMs: quiet - 1}, false},
	}
	f := newFlapStore(t)
	for _, tt := range tests {
		f.add(tt.id, tt.st)
	}
	fake := &fakeApplier{}
	w := NewFlappingWatcher(FlappingWatcherOptions{State: f.st, Cluster: fake, Now: func() time.Time { return time.UnixMilli(now) },
		Logger: slog.New(slog.DiscardHandler)})
	if err := w.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := map[string]*pb.ClearMonitorFlapping{}
	for _, c := range fake.proposed() {
		got[c.GetMonitorId()] = c
	}
	for _, tt := range tests {
		c, ok := got[tt.id]
		if ok != tt.want {
			t.Errorf("%s: proposed = %v, want %v", tt.name, ok, tt.want)
			continue
		}
		if ok && c.GetExpectedSinceMs() != tt.st.FlappingSinceMs {
			t.Errorf("%s: expected since = %d, want %d", tt.name, c.GetExpectedSinceMs(), tt.st.FlappingSinceMs)
		}
	}
}

// TestFlappingWatcherErrors: conflicts and a cluster below level 4 are normal;
// other failures are returned.
func TestFlappingWatcherErrors(t *testing.T) {
	const now = int64(10_000_000)
	f := newFlapStore(t)
	f.add("m1", store.MonitorState{Status: store.StatusUp, SinceMs: now - 40*60_000, Flapping: true, FlappingSinceMs: now - 41*60_000})
	tests := []struct {
		name    string
		err     error
		wantErr bool
	}{
		{"conflict", &cluster.CommandError{Code: cluster.CodeConflict, Message: "changed"}, false},
		{"not enabled", &cluster.CommandError{Code: cluster.CodeNotEnabled, Message: "level 3"}, false},
		{"cluster unavailable", errors.New("no leader"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeApplier{err: tt.err}
			w := NewFlappingWatcher(FlappingWatcherOptions{State: f.st, Cluster: fake, Now: func() time.Time { return time.UnixMilli(now) },
				Logger: slog.New(slog.DiscardHandler)})
			err := w.Check(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("Check() error = %v, want error %v", err, tt.wantErr)
			}
			if len(fake.proposed()) != 1 {
				t.Errorf("proposals = %d, want 1", len(fake.proposed()))
			}
		})
	}
}

// TestFlappingWatcherRunsOnLeaderOnly: Run checks only while this node leads.
func TestFlappingWatcherRunsOnLeaderOnly(t *testing.T) {
	const now = int64(10_000_000)
	f := newFlapStore(t)
	f.add("m1", store.MonitorState{Status: store.StatusUp, SinceMs: now - 40*60_000, Flapping: true, FlappingSinceMs: now - 41*60_000})
	for _, leader := range []bool{false, true} {
		fake := &fakeApplier{}
		w := NewFlappingWatcher(FlappingWatcherOptions{State: f.st, Cluster: fake, Interval: time.Millisecond,
			IsLeader: func() bool { return leader }, Now: func() time.Time { return time.UnixMilli(now) },
			Logger: slog.New(slog.DiscardHandler)})
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		w.Run(ctx)
		cancel()
		if got := len(fake.proposed()) > 0; got != leader {
			t.Errorf("leader=%v: proposed = %v", leader, got)
		}
	}
}
