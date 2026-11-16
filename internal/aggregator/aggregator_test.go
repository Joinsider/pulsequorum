// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package aggregator

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/scheduler"
	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite"
)

const nodeID = "66666666-6666-6666-6666-666666666666"

// base is far in the future, so that monitors created by the test (CreatedMs
// is real time) start before the rounds the test evaluates.
var base = time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)

type fixture struct {
	t       *testing.T
	ctx     context.Context
	state   *sqlite.StateStore
	metrics *sqlite.MetricsStore
	node    *cluster.Node
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

func doc(retries, recovery int, paused bool) cluster.MonitorDocument {
	return cluster.MonitorDocument{Name: "web", Type: "http", IntervalSeconds: 60, TimeoutSeconds: 10, Retries: retries,
		RecoveryThreshold: recovery, Assignment: store.Assignment{Mode: "all"}, ConsensusPolicy: "majority", Paused: paused}
}

// recorder keeps the commands the aggregator proposes and passes them on.
type recorder struct {
	inner Applier
	cmds  []*pb.Command
}

func (r *recorder) Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error) {
	r.cmds = append(r.cmds, cmd)
	return r.inner.Apply(ctx, cmd)
}

// rig drives an Aggregator with a fake clock. Rounds of monitor "m1" (60 s
// interval, 10 s timeout, deadline 15 s) are addressed by their index: r.slot(i).
type rig struct {
	t        *testing.T
	f        *fixture
	a        *Aggregator
	rec      *recorder
	now      time.Time
	leader   bool
	registry []store.Node
	phase    int64 // ms
	k0       int64 // slot index of round 0
	inc      string
	seq      uint64
}

const intervalMs = 60_000

func newRig(t *testing.T, d cluster.MonitorDocument) *rig {
	t.Helper()
	f := newFixture(t)
	r := &rig{t: t, f: f, now: base, leader: true, inc: f.metrics.Incarnation()}
	r.rec = &recorder{inner: f.node}
	m := f.putMonitor(d, 0)
	r.phase = scheduler.Phase("m1", time.Minute).Milliseconds()
	r.k0 = floorDiv(base.UnixMilli()-r.phase, intervalMs) + 1
	r.a = New(Options{State: f.state, Metrics: f.metrics, Cluster: r.rec, NodeID: nodeID,
		IsLeader: func() bool { return r.leader },
		Registry: func(context.Context) ([]store.Node, error) { return r.registry, nil },
		Now:      func() time.Time { return r.now },
		Logger:   slog.New(slog.DiscardHandler)})
	// Tests skip the grace period unless they test it (zero is not a default
	// after New, and a fake clock does not advance within one step).
	r.a.opts.LeaderGrace = 0
	r.a.Sync([]store.Monitor{m})
	return r
}

// slot returns the start of round i in Unix milliseconds.
func (r *rig) slot(i int64) int64 { return r.phase + (r.k0+i)*intervalMs }

// hb returns a heartbeat of node for round i, off milliseconds after its start.
func (r *rig) hb(node string, i, off int64, st store.CheckStatus) store.Heartbeat {
	r.seq++
	h := store.Heartbeat{NodeID: node, Incarnation: r.inc, Seq: r.seq, TenantID: cluster.DefaultTenantID,
		MonitorID: "m1", TsMs: r.slot(i) + off, Status: st}
	if st == store.CheckDown {
		h.Message, h.ErrorClass = "connection refused", "connect"
	}
	return h
}

// deliver stores heartbeats, then passes them on as the heartbeat writer and
// the replication do, and runs one evaluation at atMs.
func (r *rig) deliver(atMs int64, hbs ...store.Heartbeat) {
	r.t.Helper()
	r.now = time.UnixMilli(atMs)
	if _, err := r.f.metrics.InsertHeartbeats(r.f.ctx, hbs); err != nil {
		r.t.Fatal(err)
	}
	r.a.absorb(hbs)
	r.a.step(r.f.ctx)
}

// tick runs one evaluation at atMs without new heartbeats.
func (r *rig) tick(atMs int64) {
	r.now = time.UnixMilli(atMs)
	r.a.step(r.f.ctx)
}

func (r *rig) wantCommands(n int) {
	r.t.Helper()
	if len(r.rec.cmds) != n {
		r.t.Fatalf("proposed %d commands, want %d", len(r.rec.cmds), n)
	}
}

func (r *rig) nodes(ids ...string) []store.Node {
	out := make([]store.Node, 0, len(ids))
	for _, id := range ids {
		out = append(out, store.Node{ID: id, Role: store.NodeRoleVoter, Healthy: true})
	}
	return out
}

// TestRoundEvaluatedWhenAllReported checks that a round is evaluated as soon as
// every responsible node reported, without waiting for the deadline (ADR 0021).
func TestRoundEvaluatedWhenAllReported(t *testing.T) {
	r := newRig(t, doc(0, 1, false))
	r.registry = r.nodes("n1", "n2", "n3")
	r.deliver(r.slot(0)+100, r.hb("n1", 0, 100, store.CheckUp), r.hb("n2", 0, 100, store.CheckUp), r.hb("n3", 0, 100, store.CheckUp))
	r.f.expect(store.StatusUp, r.slot(0))
	r.wantCommands(1)
}

// TestRoundWaitsForDeadline checks that a round with a missing node is
// evaluated at its deadline: timeout plus 5 s, at most one interval.
func TestRoundWaitsForDeadline(t *testing.T) {
	r := newRig(t, doc(0, 1, false))
	r.registry = r.nodes("n1", "n2", "n3")
	r.deliver(r.slot(0)+100, r.hb("n1", 0, 100, store.CheckUp), r.hb("n2", 0, 100, store.CheckUp))
	r.tick(r.slot(0) + 1000)
	r.tick(r.slot(0) + 15000 - 1)
	r.wantCommands(0)
	r.tick(r.slot(0) + 15000)
	r.f.expect(store.StatusUp, r.slot(0))
	r.wantCommands(1)
}

// TestLateReportCountsForNextRound checks that a heartbeat arriving after the
// deadline does not change its round, but is the latest report of its node for
// the next round (FA-CON-05).
func TestLateReportCountsForNextRound(t *testing.T) {
	r := newRig(t, doc(0, 1, false))
	r.registry = r.nodes("n1", "n2", "n3")
	r.deliver(r.slot(0)+100, r.hb("n1", 0, 100, store.CheckUp), r.hb("n2", 0, 100, store.CheckUp))
	r.tick(r.slot(0) + 15000)
	r.f.expect(store.StatusUp, r.slot(0))

	// n3 reports DOWN for round 0 after the deadline: round 0 stays UP.
	r.deliver(r.slot(0)+16000, r.hb("n3", 0, 500, store.CheckDown))
	r.f.expect(store.StatusUp, r.slot(0))

	// Round 1: n1 and n2 are UP, n3 is missing, so its round 0 report counts.
	// One DOWN of three is PARTIAL.
	r.deliver(r.slot(1)+100, r.hb("n1", 1, 100, store.CheckUp), r.hb("n2", 1, 100, store.CheckUp))
	r.tick(r.slot(1) + 15000)
	r.f.expect(store.StatusPartial, r.slot(1))
	r.wantCommands(2)
}

// TestRetriesCountRoundsNotReports checks that three DOWN reports in one round
// count as one failed round (FA-CON-03): with retries 1 the monitor is PENDING.
func TestRetriesCountRoundsNotReports(t *testing.T) {
	r := newRig(t, doc(1, 1, false))
	r.registry = r.nodes("n1", "n2", "n3")
	r.deliver(r.slot(0)+100, r.hb("n1", 0, 100, store.CheckDown), r.hb("n2", 0, 100, store.CheckDown), r.hb("n3", 0, 100, store.CheckDown))
	if st := r.f.monitor().State.Status; st != store.StatusPending {
		t.Fatalf("status after one failed round = %s, want PENDING", st)
	}
	r.wantCommands(0)

	r.deliver(r.slot(1)+100, r.hb("n1", 1, 100, store.CheckDown), r.hb("n2", 1, 100, store.CheckDown), r.hb("n3", 1, 100, store.CheckDown))
	r.f.expect(store.StatusDown, r.slot(0))
	r.wantCommands(1)
	page, err := r.f.state.ListIncidents(r.f.ctx, cluster.DefaultTenantID, store.IncidentQuery{OpenOnly: true})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("open incidents = %+v, %v", page.Items, err)
	}
	inc := page.Items[0]
	if inc.StartedMs != r.slot(0) || inc.Cause != "connection refused" || inc.ErrorClass != "connect" ||
		!equalStrings(inc.AffectedNodes, []string{"n1", "n2", "n3"}) {
		t.Fatalf("incident = %+v", inc)
	}
}

// TestPartialWithOneDown checks that one DOWN of three under majority is PARTIAL.
func TestPartialWithOneDown(t *testing.T) {
	r := newRig(t, doc(0, 1, false))
	r.registry = r.nodes("n1", "n2", "n3")
	r.deliver(r.slot(0)+100, r.hb("n1", 0, 100, store.CheckDown), r.hb("n2", 0, 100, store.CheckUp), r.hb("n3", 0, 100, store.CheckUp))
	r.f.expect(store.StatusPartial, r.slot(0))
	r.wantCommands(1)
}

// TestUnknownWhenNobodyReports checks that a monitor without reports becomes
// UNKNOWN once its last report is older than two intervals, and that DOWN
// stays DOWN without reports (FA-CON-05, ADR 0021).
func TestUnknownWhenNobodyReports(t *testing.T) {
	r := newRig(t, doc(0, 1, false))
	r.deliver(r.slot(0)+100, r.hb(nodeID, 0, 100, store.CheckUp))
	// Round 1 still sees the report of round 0; at the deadline of round 2 it
	// is older than two intervals and counts as missing (FA-CON-05).
	r.tick(r.slot(1) + 15000)
	r.f.expect(store.StatusUp, r.slot(0))
	r.tick(r.slot(2) + 15000)
	r.f.expect(store.StatusUnknown, r.slot(2))

	r.deliver(r.slot(4)+100, r.hb(nodeID, 4, 100, store.CheckDown))
	r.f.expect(store.StatusDown, r.slot(4))
	for i := int64(5); i <= 7; i++ {
		r.tick(r.slot(i) + 15000)
	}
	r.f.expect(store.StatusDown, r.slot(4))
	r.wantCommands(3)
}

// TestFollowerProposesNothing checks that a node that is not the leader only
// stores heartbeats and proposes no status change (FA-CON-06).
func TestFollowerProposesNothing(t *testing.T) {
	r := newRig(t, doc(0, 0, false))
	r.leader = false
	r.registry = r.nodes("n1", "n2")
	r.deliver(r.slot(0)+100, r.hb("n1", 0, 100, store.CheckDown), r.hb("n2", 0, 100, store.CheckDown))
	r.tick(r.slot(0) + 15000)
	r.tick(r.slot(1) + 15000)
	r.f.expect(store.StatusPending, r.f.monitor().State.SinceMs)
	r.wantCommands(0)
	if w := r.a.watches[monitorKey{cluster.DefaultTenantID, "m1"}]; w.ready {
		t.Fatal("follower keeps an evaluation state")
	}
}

// TestNewLeaderRebuildsSameDecision checks that a new leader waits for the
// grace period, rebuilds the streaks from the stored heartbeats and reaches the
// decision the previous leader would have made (NFA-REL-03, ADR 0021).
func TestNewLeaderRebuildsSameDecision(t *testing.T) {
	r := newRig(t, doc(1, 1, false))
	r.a.opts.LeaderGrace = 10 * time.Second
	r.leader = false
	// The previous leader sees two failed rounds: PENDING, then DOWN since round 0.
	r.deliver(r.slot(0)+100, r.hb(nodeID, 0, 100, store.CheckDown))
	r.deliver(r.slot(1)+100, r.hb(nodeID, 1, 100, store.CheckDown))
	r.wantCommands(0)

	r.leader = true
	r.tick(r.slot(1) + 200) // leadership starts
	r.tick(r.slot(1) + 5000)
	r.wantCommands(0)
	r.tick(r.slot(1) + 10200) // grace period over: rebuild and replay
	r.f.expect(store.StatusDown, r.slot(0))
	r.wantCommands(1)
	page, err := r.f.state.ListIncidents(r.f.ctx, cluster.DefaultTenantID, store.IncidentQuery{OpenOnly: true})
	if err != nil || len(page.Items) != 1 || page.Items[0].StartedMs != r.slot(0) ||
		!equalStrings(page.Items[0].AffectedNodes, []string{nodeID}) {
		t.Fatalf("incident = %+v, %v", page.Items, err)
	}
}

// TestMaintenanceRoundsResetStreaks checks that a round in maintenance is not
// evaluated and that the streaks before it do not count (FA-MNT-02).
func TestMaintenanceRoundsResetStreaks(t *testing.T) {
	r := newRig(t, doc(1, 1, false))
	r.a.opts.Maintenance = func(_ store.Monitor, at time.Time) bool { return at.UnixMilli() == r.slot(0) }
	r.deliver(r.slot(0)+100, r.hb(nodeID, 0, 100, store.CheckDown))
	r.deliver(r.slot(1)+100, r.hb(nodeID, 1, 100, store.CheckDown))
	r.f.expect(store.StatusPending, r.f.monitor().State.SinceMs)
	r.wantCommands(0)
	r.deliver(r.slot(2)+100, r.hb(nodeID, 2, 100, store.CheckDown))
	r.f.expect(store.StatusDown, r.slot(1))
	r.wantCommands(1)
}

// TestTenantBoundary checks that a heartbeat of another tenant with the same
// monitor ID is ignored (FA-TEN, ADR 0005).
func TestTenantBoundary(t *testing.T) {
	r := newRig(t, doc(0, 1, false))
	foreign := r.hb(nodeID, 0, 100, store.CheckDown)
	foreign.TenantID = "other-tenant"
	r.deliver(r.slot(0)+100, foreign)
	r.tick(r.slot(0) + 15000)
	r.f.expect(store.StatusPending, r.f.monitor().State.SinceMs)
	r.wantCommands(0)

	r.deliver(r.slot(1)+100, r.hb(nodeID, 1, 100, store.CheckDown))
	r.f.expect(store.StatusDown, r.slot(1))
}

// TestExcludedNodeIsNotAwaited checks that an excluded node is neither awaited
// nor counted (FA-CON-04 hook).
func TestExcludedNodeIsNotAwaited(t *testing.T) {
	r := newRig(t, doc(0, 1, false))
	r.registry = r.nodes("n1", "n2", "n3")
	r.a.opts.Exclude = func(id string) bool { return id == "n3" }
	r.deliver(r.slot(0)+100, r.hb("n1", 0, 100, store.CheckUp), r.hb("n2", 0, 100, store.CheckUp), r.hb("n3", 0, 100, store.CheckDown))
	r.f.expect(store.StatusUp, r.slot(0))
}

// TestObserveNeverBlocks checks that Observe returns at once even when nobody
// consumes the queue.
func TestObserveNeverBlocks(t *testing.T) {
	a := New(Options{Logger: slog.New(slog.DiscardHandler)})
	done := make(chan struct{})
	go func() {
		for range 3 * observeQueueSize {
			a.Observe([]store.Heartbeat{{TenantID: cluster.DefaultTenantID, MonitorID: "m1"}})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Observe blocks")
	}
}

// TestRunEvaluatesObservedHeartbeats runs the loop with the real clock: a
// heartbeat stored and observed through Run decides the round at once, and Sync
// with no monitors forgets all of them.
func TestRunEvaluatesObservedHeartbeats(t *testing.T) {
	f := newFixture(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	d := doc(0, 1, false)
	d.IntervalSeconds = 3600
	m := f.putMonitor(d, 0)
	phase := scheduler.Phase("m1", time.Hour).Milliseconds()
	// The first round that starts after the monitor was created.
	k := floorDiv(m.CreatedMs-phase, 3_600_000) + 1
	hb := store.Heartbeat{NodeID: nodeID, Incarnation: f.metrics.Incarnation(), Seq: 1, TenantID: cluster.DefaultTenantID,
		MonitorID: "m1", TsMs: phase + k*3_600_000, Status: store.CheckDown, Message: "connection refused", ErrorClass: "connect"}
	if _, err := f.metrics.InsertHeartbeats(f.ctx, []store.Heartbeat{hb}); err != nil {
		t.Fatal(err)
	}

	a := New(Options{State: f.state, Metrics: f.metrics, Cluster: f.node, NodeID: nodeID,
		IsLeader: func() bool { return true }, LeaderGrace: time.Nanosecond, Logger: slog.New(slog.DiscardHandler)})
	a.Sync([]store.Monitor{m})
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	a.Observe([]store.Heartbeat{hb})

	deadline := time.Now().Add(5 * time.Second)
	for f.monitor().State.Status != store.StatusDown {
		if time.Now().After(deadline) {
			t.Fatalf("status = %s, want DOWN", f.monitor().State.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.Sync(nil)
	a.mu.Lock()
	n := len(a.watches)
	a.mu.Unlock()
	if n != 0 {
		t.Fatalf("watches after sync = %d, want 0", n)
	}
	cancel()
	<-done
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
