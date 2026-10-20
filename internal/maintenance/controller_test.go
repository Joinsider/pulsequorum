// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package maintenance

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/checker"
	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite"
)

const nodeID = "77777777-7777-7777-7777-777777777777"

const tenantB = "tenant-b"

// fixture runs a single-node Raft cluster on SQLite state with an injected clock.
type fixture struct {
	t     *testing.T
	ctx   context.Context
	state *sqlite.StateStore
	node  *cluster.Node
	nowMs int64
	ctrl  *Controller
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
	node, err := cluster.Start(ctx, cluster.Config{NodeID: nodeID, Advertise: "n:9443", DataDir: dir,
		Timeout: 200 * time.Millisecond, Logger: log, RaftLogLevel: "error"}, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = state.Close()
	})
	if err := cluster.EnsureDefaultTenant(ctx, node, state); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, ctx: ctx, state: state, node: node}
	f.ctrl = NewController(ControllerOptions{
		State: state, Cluster: node, IsLeader: node.IsLeader, Logger: log,
		Now: func() time.Time { return time.UnixMilli(f.nowMs) },
	})
	return f
}

func (f *fixture) apply(cmd *pb.Command, err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.node.Apply(f.ctx, cmd); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) putMonitor(tenant, id string, doc cluster.MonitorDocument) store.Monitor {
	f.t.Helper()
	cmd, err := cluster.NewPutMonitor("", tenant, id, doc, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.node.Apply(f.ctx, cmd); err != nil {
		f.t.Fatal(err)
	}
	return f.monitor(tenant, id)
}

func (f *fixture) monitor(tenant, id string) store.Monitor {
	f.t.Helper()
	m, err := f.state.GetMonitor(f.ctx, tenant, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

func (f *fixture) expect(tenant, id string, status store.MonitorStatus, sinceMs int64) {
	f.t.Helper()
	if st := f.monitor(tenant, id).State; st.Status != status || st.SinceMs != sinceMs {
		f.t.Fatalf("%s/%s state = %s since %d, want %s since %d", tenant, id, st.Status, st.SinceMs, status, sinceMs)
	}
}

func (f *fixture) putWindow(tenant, id, req string, rev uint64, doc cluster.MaintenanceDocument) {
	f.t.Helper()
	cmd, err := cluster.NewPutMaintenanceWindow(req, tenant, id, doc, rev)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.node.Apply(f.ctx, cmd); err != nil {
		f.t.Fatal(err)
	}
}

// reconcile refreshes the windows and reconciles as the leader does.
func (f *fixture) reconcile(atMs int64) {
	f.t.Helper()
	f.nowMs = atMs
	if err := f.ctrl.Refresh(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	if err := f.ctrl.Reconcile(f.ctx); err != nil {
		f.t.Fatal(err)
	}
}

func monitorDoc(paused bool) cluster.MonitorDocument {
	return cluster.MonitorDocument{Name: "web", Type: "http", IntervalSeconds: 60, TimeoutSeconds: 10,
		RecoveryThreshold: 1, Assignment: store.Assignment{Mode: "all"}, ConsensusPolicy: "majority", Paused: paused}
}

func onceWindow(name string, from, to int64, monitorIDs ...string) cluster.MaintenanceDocument {
	return cluster.MaintenanceDocument{Name: name, Active: true, MonitorIDs: monitorIDs,
		Schedule: store.MaintenanceSchedule{Kind: store.ScheduleOnce, StartsMs: from, EndsMs: to}}
}

// TestReconcileEnterAndLeave covers FA-MNT-02: the monitor enters MAINTENANCE
// at the window start and returns to PENDING at the window end.
func TestReconcileEnterAndLeave(t *testing.T) {
	f := newFixture(t)
	m := f.putMonitor(cluster.DefaultTenantID, "m1", monitorDoc(false))
	b := m.CreatedMs
	f.putWindow(cluster.DefaultTenantID, "w1", "w1", 0, onceWindow("Deploy", b+1000, b+100_000, "m1"))

	f.reconcile(b + 500) // before the window
	f.expect(cluster.DefaultTenantID, "m1", store.StatusPending, b)

	f.reconcile(b + 5000) // inside the window
	f.expect(cluster.DefaultTenantID, "m1", store.StatusMaintenance, b+1000)

	f.reconcile(b + 50_000) // still inside: nothing changes
	f.expect(cluster.DefaultTenantID, "m1", store.StatusMaintenance, b+1000)

	f.reconcile(b + 120_000) // after the window
	f.expect(cluster.DefaultTenantID, "m1", store.StatusPending, b+100_000)
}

// TestDownMonitorEntersMaintenance checks that the open incident of a DOWN
// monitor is resolved when maintenance starts.
func TestDownMonitorEntersMaintenance(t *testing.T) {
	f := newFixture(t)
	m := f.putMonitor(cluster.DefaultTenantID, "m1", monitorDoc(false))
	b := m.CreatedMs
	inc := store.Incident{ID: "inc-1", Cause: "connection refused", ErrorClass: string(checker.ClassConnect),
		AffectedNodes: []string{nodeID}}
	f.apply(cluster.NewSetMonitorState(cluster.StateChange{TenantID: cluster.DefaultTenantID, MonitorID: "m1",
		From: store.StatusPending, To: store.StatusDown, SinceMs: b + 500, Incident: &inc}), nil)

	f.putWindow(cluster.DefaultTenantID, "w1", "w1", 0, onceWindow("Deploy", b+1000, b+100_000))
	f.reconcile(b + 2000)
	f.expect(cluster.DefaultTenantID, "m1", store.StatusMaintenance, b+1000)

	got, err := f.state.GetIncident(f.ctx, cluster.DefaultTenantID, "inc-1")
	if err != nil || got.ResolvedMs != b+1000 {
		t.Fatalf("incident after maintenance start = %+v, %v", got, err)
	}
}

// TestChainedWindowsExitAtUnionEnd checks that overlapping and touching
// windows keep the monitor in MAINTENANCE until the union ends.
func TestChainedWindowsExitAtUnionEnd(t *testing.T) {
	f := newFixture(t)
	m := f.putMonitor(cluster.DefaultTenantID, "m1", monitorDoc(false))
	s := m.CreatedMs + 1000
	f.putWindow(cluster.DefaultTenantID, "w1", "w1", 0, onceWindow("A", s, s+10_000))
	f.putWindow(cluster.DefaultTenantID, "w2", "w2", 0, onceWindow("B", s+5000, s+20_000))
	f.putWindow(cluster.DefaultTenantID, "w3", "w3", 0, onceWindow("C", s+20_000, s+30_000))

	f.reconcile(s + 6000)
	f.expect(cluster.DefaultTenantID, "m1", store.StatusMaintenance, s)

	f.reconcile(s + 25_000) // only C is active
	f.expect(cluster.DefaultTenantID, "m1", store.StatusMaintenance, s)

	f.reconcile(s + 31_000)
	f.expect(cluster.DefaultTenantID, "m1", store.StatusPending, s+30_000)
}

// TestRemovedWindowEndsMaintenanceNow checks that deactivating or deleting a
// window during maintenance ends the maintenance at the current time.
func TestRemovedWindowEndsMaintenanceNow(t *testing.T) {
	tests := []struct {
		name   string
		remove func(f *fixture, s int64)
	}{
		{"deactivated", func(f *fixture, s int64) {
			f.putWindow(cluster.DefaultTenantID, "w1", "w1-off", 1, cluster.MaintenanceDocument{Name: "Deploy", Active: false,
				MonitorIDs: []string{"m1"}, Schedule: store.MaintenanceSchedule{Kind: store.ScheduleOnce, StartsMs: s, EndsMs: s + 100_000}})
		}},
		{"deleted", func(f *fixture, _ int64) {
			f.apply(cluster.NewDeleteMaintenanceWindow("w1-del", cluster.DefaultTenantID, "w1"), nil)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			m := f.putMonitor(cluster.DefaultTenantID, "m1", monitorDoc(false))
			s := m.CreatedMs + 1000
			f.putWindow(cluster.DefaultTenantID, "w1", "w1", 0, onceWindow("Deploy", s, s+100_000, "m1"))
			f.reconcile(s + 5000)
			f.expect(cluster.DefaultTenantID, "m1", store.StatusMaintenance, s)

			tt.remove(f, s)
			f.reconcile(s + 7000)
			f.expect(cluster.DefaultTenantID, "m1", store.StatusPending, s+7000)
		})
	}
}

// TestPausedMonitorUntouched checks that a paused monitor keeps its status.
func TestPausedMonitorUntouched(t *testing.T) {
	f := newFixture(t)
	m := f.putMonitor(cluster.DefaultTenantID, "m1", monitorDoc(true))
	f.expect(cluster.DefaultTenantID, "m1", store.StatusPaused, m.CreatedMs)
	f.putWindow(cluster.DefaultTenantID, "w1", "w1", 0, onceWindow("Deploy", m.CreatedMs, m.CreatedMs+100_000))

	f.reconcile(m.CreatedMs + 5000)
	f.expect(cluster.DefaultTenantID, "m1", store.StatusPaused, m.CreatedMs)
}

// TestWindowsStayInTheirTenant checks that a window of tenant B with no monitor
// IDs never applies to the monitors of the default tenant.
func TestWindowsStayInTheirTenant(t *testing.T) {
	f := newFixture(t)
	mA := f.putMonitor(cluster.DefaultTenantID, "m1", monitorDoc(false))
	tenantCmd, err := cluster.NewPutTenant("tenant-b-create", tenantB, cluster.TenantDocument{Name: "Tenant B", Plan: "default"})
	f.apply(tenantCmd, err)
	mB := f.putMonitor(tenantB, "b1", monitorDoc(false))
	f.putWindow(tenantB, "wb", "wb", 0, onceWindow("All of B", mB.CreatedMs, mB.CreatedMs+100_000))

	f.reconcile(mB.CreatedMs + 5000)
	f.expect(cluster.DefaultTenantID, "m1", store.StatusPending, mA.CreatedMs)
	f.expect(tenantB, "b1", store.StatusMaintenance, mB.CreatedMs)

	if f.ctrl.InMaintenance(mA, time.UnixMilli(mB.CreatedMs+5000)) {
		t.Fatal("window of tenant B applies to a monitor of the default tenant")
	}
	if !f.ctrl.InMaintenance(mB, time.UnixMilli(mB.CreatedMs+5000)) {
		t.Fatal("window of tenant B does not apply to its own monitor")
	}
}

// TestInMaintenance checks the cached windows as the aggregator sees them.
func TestInMaintenance(t *testing.T) {
	f := newFixture(t)
	m := f.putMonitor(cluster.DefaultTenantID, "m1", monitorDoc(false))
	other := f.putMonitor(cluster.DefaultTenantID, "m2", monitorDoc(false))
	s := m.CreatedMs + 1000
	f.putWindow(cluster.DefaultTenantID, "w1", "w1", 0, onceWindow("Deploy", s, s+10_000, "m1"))
	if err := f.ctrl.Refresh(f.ctx); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		m    store.Monitor
		at   int64
		want bool
	}{
		{"before the window", m, s - 1, false},
		{"at the start", m, s, true},
		{"inside", m, s + 5000, true},
		{"at the end (exclusive)", m, s + 10_000, false},
		{"other monitor of the tenant", other, s + 5000, false},
	}
	for _, tt := range tests {
		if got := f.ctrl.InMaintenance(tt.m, time.UnixMilli(tt.at)); got != tt.want {
			t.Errorf("%s: InMaintenance = %v, want %v", tt.name, got, tt.want)
		}
	}

	// Deactivated windows are not cached.
	f.putWindow(cluster.DefaultTenantID, "w1", "w1-off", 1, cluster.MaintenanceDocument{Name: "Deploy", Active: false,
		MonitorIDs: []string{"m1"}, Schedule: store.MaintenanceSchedule{Kind: store.ScheduleOnce, StartsMs: s, EndsMs: s + 10_000}})
	if err := f.ctrl.Refresh(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.ctrl.InMaintenance(m, time.UnixMilli(s+5000)) {
		t.Fatal("deactivated window still applies")
	}
}

// TestRunOnlyReconcilesOnLeader checks that a follower refreshes its windows
// but proposes nothing.
func TestRunOnlyReconcilesOnLeader(t *testing.T) {
	f := newFixture(t)
	m := f.putMonitor(cluster.DefaultTenantID, "m1", monitorDoc(false))
	s := m.CreatedMs + 1000
	f.putWindow(cluster.DefaultTenantID, "w1", "w1", 0, onceWindow("Deploy", s, s+100_000, "m1"))
	f.nowMs = s + 5000

	var leader atomic.Bool
	ctrl := NewController(ControllerOptions{
		State: f.state, Cluster: f.node, Logger: slog.New(slog.DiscardHandler), Interval: 5 * time.Millisecond,
		IsLeader: leader.Load,
		Now:      func() time.Time { return time.UnixMilli(f.nowMs) },
	})
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { ctrl.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(5 * time.Second)
	for !ctrl.InMaintenance(m, time.UnixMilli(s+5000)) {
		if time.Now().After(deadline) {
			t.Fatal("follower did not refresh the windows")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.expect(cluster.DefaultTenantID, "m1", store.StatusPending, m.CreatedMs)

	leader.Store(true)
	deadline = time.Now().Add(5 * time.Second)
	for f.monitor(cluster.DefaultTenantID, "m1").State.Status != store.StatusMaintenance {
		if time.Now().After(deadline) {
			t.Fatal("leader did not reconcile")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.expect(cluster.DefaultTenantID, "m1", store.StatusMaintenance, s)
}

// countingHandler counts the warnings it receives.
type countingHandler struct{ warnings *atomic.Int64 }

func (h countingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		h.warnings.Add(1)
	}
	return nil
}

func (h countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h countingHandler) WithGroup(string) slog.Handler { return h }

// TestRefreshWarnsOncePerRevision checks that an invalid window is reported
// once per revision and again only after it was removed.
func TestRefreshWarnsOncePerRevision(t *testing.T) {
	f := newFixture(t)
	var warnings atomic.Int64
	ctrl := NewController(ControllerOptions{State: f.state, Cluster: f.node, Logger: slog.New(countingHandler{&warnings})})
	broken := func(cron string) cluster.MaintenanceDocument {
		return cluster.MaintenanceDocument{Name: "Broken", Active: true,
			Schedule: store.MaintenanceSchedule{Kind: store.ScheduleRecurring, Cron: cron, DurationMinutes: 60}}
	}
	refresh := func(want int64) {
		t.Helper()
		if err := ctrl.Refresh(f.ctx); err != nil {
			t.Fatal(err)
		}
		if got := warnings.Load(); got != want {
			t.Fatalf("warnings = %d, want %d", got, want)
		}
	}

	f.putWindow(cluster.DefaultTenantID, "w1", "w1", 0, broken("not a cron"))
	refresh(1)
	refresh(1) // same revision: no new warning
	f.putWindow(cluster.DefaultTenantID, "w1", "w1-v2", 1, broken("also bad"))
	refresh(2) // new revision
	f.apply(cluster.NewDeleteMaintenanceWindow("w1-del", cluster.DefaultTenantID, "w1"), nil)
	refresh(2) // window gone: its key is forgotten
	f.putWindow(cluster.DefaultTenantID, "w1", "w1-again", 0, broken("not a cron"))
	refresh(3) // recreated at revision 1, reported again
}
