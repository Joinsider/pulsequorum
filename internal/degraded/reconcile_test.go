// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package degraded

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/aggregator"
	"github.com/joinsider/uptimecluster/internal/alert"
	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/replication"
	"github.com/joinsider/uptimecluster/internal/store"
)

var rjBase = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func rjMs(t time.Time) int64 { return t.UnixMilli() }

// rjProposer records the proposed commands. fail, if set, decides the error of a command.
type rjProposer struct {
	mu   sync.Mutex
	cmds []*pb.Command
	fail func(cmd *pb.Command) error
}

func (p *rjProposer) Apply(_ context.Context, cmd *pb.Command) (cluster.ApplyResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cmds = append(p.cmds, cmd)
	if p.fail != nil {
		if err := p.fail(cmd); err != nil {
			return cluster.ApplyResult{}, err
		}
	}
	return cluster.ApplyResult{}, nil
}

func (p *rjProposer) all() []*pb.Command {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.cmds)
}

// rjState is the replicated state with the reads of the judge and the gap job.
// Methods that are not overridden panic through the nil embedded interface.
type rjState struct {
	store.State

	mu         sync.Mutex
	tenants    []store.Tenant
	monitors   []store.Monitor
	history    map[string][]store.StatusChange // by tenant/monitor
	outbox     map[string]bool                 // by channel/alert key
	open       []store.UnconfirmedAlert
	lastIssued int64
	windows    [][2]int64 // StatusHistory calls
}

func rjKey(tenant, id string) string { return tenant + "/" + id }

func (s *rjState) ListOpenUnconfirmedAlerts(_ context.Context, limit int) ([]store.UnconfirmedAlert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.open[:min(limit, len(s.open))]), nil
}

func (s *rjState) GetMonitor(_ context.Context, tenant, id string) (store.Monitor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.monitors {
		if m.TenantID == tenant && m.ID == id {
			return m, nil
		}
	}
	return store.Monitor{}, store.ErrNotFound
}

func (s *rjState) StatusHistory(_ context.Context, tenant, id string, from, to int64) ([]store.StatusChange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.windows = append(s.windows, [2]int64{from, to})
	return slices.Clone(s.history[rjKey(tenant, id)]), nil
}

func (s *rjState) GetAlert(_ context.Context, key, channel string) (store.AlertDelivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outbox[channel+"|"+key] {
		return store.AlertDelivery{AlertKey: key, ChannelID: channel}, nil
	}
	return store.AlertDelivery{}, store.ErrNotFound
}

func (s *rjState) LastIssuedAt(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastIssued, nil
}

func (s *rjState) ListTenants(context.Context) ([]store.Tenant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.tenants), nil
}

// ListMonitors pages by ID with the query limit.
func (s *rjState) ListMonitors(_ context.Context, tenant string, q store.MonitorQuery) (store.MonitorPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []store.Monitor
	for _, m := range s.monitors {
		if m.TenantID == tenant && m.ID > q.AfterID {
			all = append(all, m)
		}
	}
	slices.SortFunc(all, func(a, b store.Monitor) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	if q.Limit > 0 && len(all) > q.Limit {
		return store.MonitorPage{Items: all[:q.Limit], NextAfterID: all[q.Limit-1].ID}, nil
	}
	return store.MonitorPage{Items: all}, nil
}

// rjMetrics is the node-local log. Unreported alerts are returned by ID.
type rjMetrics struct {
	store.Metrics

	mu         sync.Mutex
	unreported []store.DegradedAlert
	marked     []int64
	pruned     []int64
}

func (m *rjMetrics) ListUnreportedDegradedAlerts(_ context.Context, limit int) ([]store.DegradedAlert, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.unreported[:min(limit, len(m.unreported))]), nil
}

func (m *rjMetrics) MarkDegradedAlertsReported(_ context.Context, ids []int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.marked = append(m.marked, ids...)
	m.unreported = slices.DeleteFunc(m.unreported, func(a store.DegradedAlert) bool { return slices.Contains(ids, a.ID) })
	return nil
}

func (m *rjMetrics) PruneDegradedAlerts(_ context.Context, beforeMs int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruned = append(m.pruned, beforeMs)
	return 0, nil
}

// rjReeval returns the changes per monitor ID and records the gap requests.
type rjReeval struct {
	mu       sync.Mutex
	changes  map[string][]aggregator.Change
	requests []rjGapRequest
}

type rjGapRequest struct {
	monitor string
	from    store.MonitorStatus
	fromMs  int64
	toMs    int64
}

func (r *rjReeval) ReevaluateGap(_ context.Context, m store.Monitor, from store.MonitorStatus, fromMs, toMs int64) ([]aggregator.Change, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, rjGapRequest{monitor: m.ID, from: from, fromMs: fromMs, toMs: toMs})
	return r.changes[m.ID], nil
}

func rjCmdLevel(level uint32) func(context.Context) (uint32, error) {
	return func(context.Context) (uint32, error) { return level, nil }
}

func rjMonitor(id string, status store.MonitorStatus, since time.Time) store.Monitor {
	return store.Monitor{
		TenantID: testTenant, ID: id, Name: id, Type: "http",
		IntervalSeconds: 60, Retries: 1, RecoveryThreshold: 1,
		State: store.MonitorState{Status: status, SinceMs: rjMs(since), Confirmed: true},
	}
}

func resolvedVerdicts(t *testing.T, cmd *pb.Command) []*pb.UnconfirmedVerdict {
	t.Helper()
	r := cmd.GetResolveUnconfirmedAlerts()
	if r == nil {
		t.Fatalf("command %T, want ResolveUnconfirmedAlerts", cmd.GetPayload())
	}
	return r.GetVerdicts()
}

// ---------------------------------------------------------------- Reporter

func TestReporterSendsBatchesAndMarks(t *testing.T) {
	metrics := &rjMetrics{}
	for i := range 250 {
		metrics.unreported = append(metrics.unreported, store.DegradedAlert{
			ID: int64(i + 1), TenantID: testTenant, MonitorID: "m1", ChannelID: "ch", Status: "DOWN", SinceMs: rjMs(rjBase),
			Outcome: alert.LocalOutcomeSent, Reachable: 1, Total: 3,
		})
	}
	prop := &rjProposer{}
	r := NewReporter(ReporterOptions{
		NodeID: testNode, Detector: func() State { return State{} }, LeaderID: func() string { return "node-b" },
		CaughtUp: func() bool { return true }, CommandLevel: rjCmdLevel(cluster.DegradedCommandLevel),
		Proposer: prop, Metrics: metrics, Now: func() time.Time { return rjBase }, Logger: rjLogger(),
	})
	r.tick(context.Background())

	cmds := prop.all()
	if len(cmds) != 3 {
		t.Fatalf("%d commands, want 3 batches (100, 100, 50)", len(cmds))
	}
	wantCounts := []int{100, 100, 50}
	for i, cmd := range cmds {
		rep := cmd.GetReportDegradedAlerts()
		if rep == nil || rep.GetNodeId() != testNode || len(rep.GetEntries()) != wantCounts[i] {
			t.Errorf("command %d: report %+v, want node %s and %d entries", i, rep, testNode, wantCounts[i])
		}
	}
	if len(metrics.marked) != 250 || len(metrics.unreported) != 0 {
		t.Errorf("marked %d, unreported left %d; want 250 and 0", len(metrics.marked), len(metrics.unreported))
	}
}

func TestReporterGates(t *testing.T) {
	cases := []struct {
		name     string
		detector State
		leader   string
		caughtUp bool
		level    uint32
	}{
		{name: "degraded", detector: State{Degraded: true}, leader: "node-b", caughtUp: true, level: 5},
		{name: "no leader", leader: "", caughtUp: true, level: 5},
		{name: "not caught up", leader: "node-b", caughtUp: false, level: 5},
		{name: "level 4", leader: "node-b", caughtUp: true, level: 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metrics := &rjMetrics{unreported: []store.DegradedAlert{{ID: 1, MonitorID: "m1", ChannelID: "ch", Status: "DOWN"}}}
			prop := &rjProposer{}
			r := NewReporter(ReporterOptions{
				NodeID: testNode, Detector: func() State { return tc.detector }, LeaderID: func() string { return tc.leader },
				CaughtUp: func() bool { return tc.caughtUp }, CommandLevel: rjCmdLevel(tc.level),
				Proposer: prop, Metrics: metrics, Now: func() time.Time { return rjBase }, Logger: rjLogger(),
			})
			r.tick(context.Background())
			if n := len(prop.all()); n != 0 {
				t.Errorf("%d commands sent, want none", n)
			}
			if len(metrics.marked) != 0 {
				t.Errorf("marked %v, want none", metrics.marked)
			}
		})
	}
}

func TestReporterKeepsAlertsOnError(t *testing.T) {
	metrics := &rjMetrics{unreported: []store.DegradedAlert{{ID: 1, MonitorID: "m1", ChannelID: "ch", Status: "UP"}}}
	prop := &rjProposer{fail: func(*pb.Command) error { return errors.New("no quorum") }}
	r := NewReporter(ReporterOptions{
		NodeID: testNode, Detector: func() State { return State{} }, LeaderID: func() string { return "node-b" },
		CaughtUp: func() bool { return true }, CommandLevel: rjCmdLevel(5),
		Proposer: prop, Metrics: metrics, Now: func() time.Time { return rjBase }, Logger: rjLogger(),
	})
	r.tick(context.Background())
	if len(metrics.marked) != 0 || len(metrics.unreported) != 1 {
		t.Errorf("after failed apply: marked %v, unreported %d; want none marked and one left", metrics.marked, len(metrics.unreported))
	}
}

func TestReporterPrunesOncePerDay(t *testing.T) {
	metrics := &rjMetrics{}
	now := rjBase
	r := NewReporter(ReporterOptions{
		NodeID: testNode, Detector: func() State { return State{Degraded: true} }, LeaderID: func() string { return "" },
		CaughtUp: func() bool { return false }, CommandLevel: rjCmdLevel(5),
		Proposer: &rjProposer{}, Metrics: metrics, Now: func() time.Time { return now }, Logger: rjLogger(),
	})
	r.tick(context.Background())
	now = rjBase.Add(time.Hour)
	r.tick(context.Background())
	now = rjBase.Add(25 * time.Hour)
	r.tick(context.Background())

	want := []int64{rjMs(rjBase.Add(-logRetention)), rjMs(rjBase.Add(25*time.Hour - logRetention))}
	if !slices.Equal(metrics.pruned, want) {
		t.Errorf("pruned before %v, want %v (once at start, once after a day)", metrics.pruned, want)
	}
}

// ---------------------------------------------------------------- Judge

// rjAlert returns an open alert; the status is "DOWN" or "UP".
func rjAlert(node string, local int64, monitor, status string, since time.Time, outcome string) store.UnconfirmedAlert {
	return store.UnconfirmedAlert{
		NodeID: node, LocalID: local, TenantID: testTenant, MonitorID: monitor, ChannelID: "ch",
		Status: status, SinceMs: rjMs(since), SentMs: rjMs(since), Outcome: outcome, Reachable: 1, Total: 3,
	}
}

// rjJudge builds a judge over st with the gap job done and command level 5.
func rjJudge(st *rjState, prop *rjProposer, now time.Time, retention time.Duration) *Judge {
	return NewJudge(JudgeOptions{
		State: st, Proposer: prop, IsLeader: func() bool { return true },
		CommandLevel: rjCmdLevel(cluster.DegradedCommandLevel), GapDone: func() bool { return true },
		RetentionRaw: retention, Now: func() time.Time { return now }, Logger: rjLogger(),
	})
}

func TestJudgeWindowNotConfirmedIsSkipped(t *testing.T) {
	// The window ends 2 min after the alert (retries 1): not yet settled.
	since := rjBase.Add(-time.Minute)
	st := &rjState{
		monitors: []store.Monitor{rjMonitor("m1", store.StatusUp, rjBase.Add(-time.Hour))},
		open:     []store.UnconfirmedAlert{rjAlert("node-a", 1, "m1", "DOWN", since, alert.LocalOutcomeSent)},
	}
	prop := &rjProposer{}
	rjJudge(st, prop, rjBase, 0).tick(context.Background())
	if n := len(prop.all()); n != 0 {
		t.Errorf("%d commands, want none while the window is open", n)
	}
}

func TestJudgeConfirmedAndFalseAlarm(t *testing.T) {
	since := rjBase.Add(-time.Hour)
	st := &rjState{
		monitors: []store.Monitor{rjMonitor("m1", store.StatusUp, rjBase.Add(-2*time.Hour)), rjMonitor("m2", store.StatusUp, rjBase.Add(-2*time.Hour))},
		history: map[string][]store.StatusChange{
			rjKey(testTenant, "m1"): {{SinceMs: rjMs(since), Status: store.StatusDown}},
			// m2 has no DOWN in its window: the DOWN was a false alarm.
		},
		open: []store.UnconfirmedAlert{
			rjAlert("node-a", 1, "m1", "DOWN", since, alert.LocalOutcomeSent),
			rjAlert("node-a", 2, "m2", "DOWN", since, alert.LocalOutcomeSent),
		},
	}
	prop := &rjProposer{}
	rjJudge(st, prop, rjBase, 0).tick(context.Background())

	cmds := prop.all()
	if len(cmds) != 1 {
		t.Fatalf("%d commands, want 1 resolve", len(cmds))
	}
	v := resolvedVerdicts(t, cmds[0])
	if len(v) != 2 {
		t.Fatalf("%d verdicts, want 2", len(v))
	}
	if v[0].GetVerdict() != store.VerdictConfirmed || !v[0].GetNotify() {
		t.Errorf("m1: verdict %q notify %v, want confirmed and notified (no outbox entry)", v[0].GetVerdict(), v[0].GetNotify())
	}
	if v[1].GetVerdict() != store.VerdictFalseAlarm || !v[1].GetNotify() {
		t.Errorf("m2: verdict %q notify %v, want false_alarm notified (outcome sent)", v[1].GetVerdict(), v[1].GetNotify())
	}
}

func TestJudgeNotifyRules(t *testing.T) {
	since := rjBase.Add(-time.Hour)
	confirmedHistory := map[string][]store.StatusChange{
		rjKey(testTenant, "m1"): {{SinceMs: rjMs(since), Status: store.StatusDown}},
	}
	cases := []struct {
		name       string
		alert      store.UnconfirmedAlert
		history    map[string][]store.StatusChange
		outbox     map[string]bool
		wantVerd   string
		wantNotify bool
	}{
		{
			name:       "confirmed, sent, no outbox entry",
			alert:      rjAlert("node-a", 1, "m1", "DOWN", since, alert.LocalOutcomeSent),
			history:    confirmedHistory,
			wantVerd:   store.VerdictConfirmed,
			wantNotify: true,
		},
		{
			name:     "confirmed, channel already has the alert",
			alert:    rjAlert("node-a", 1, "m1", "DOWN", since, alert.LocalOutcomeSent),
			history:  confirmedHistory,
			outbox:   map[string]bool{"ch|" + alertKey("m1", rjMs(since), store.StatusDown): true},
			wantVerd: store.VerdictConfirmed, wantNotify: false,
		},
		{
			name:     "confirmed, own alert was not sent",
			alert:    rjAlert("node-a", 1, "m1", "DOWN", since, alert.LocalOutcomeFailed),
			history:  confirmedHistory,
			wantVerd: store.VerdictConfirmed, wantNotify: false,
		},
		{
			name:     "false alarm, sent",
			alert:    rjAlert("node-a", 1, "m1", "UP", since, alert.LocalOutcomeSent),
			history:  confirmedHistory,
			wantVerd: store.VerdictFalseAlarm, wantNotify: true,
		},
		{
			name:     "false alarm, not sent",
			alert:    rjAlert("node-a", 1, "m1", "UP", since, alert.LocalOutcomeFailed),
			history:  confirmedHistory,
			wantVerd: store.VerdictFalseAlarm, wantNotify: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &rjState{
				monitors: []store.Monitor{rjMonitor("m1", store.StatusUp, rjBase.Add(-2*time.Hour))},
				history:  tc.history, outbox: tc.outbox,
				open: []store.UnconfirmedAlert{tc.alert},
			}
			prop := &rjProposer{}
			rjJudge(st, prop, rjBase, 0).tick(context.Background())
			v := resolvedVerdicts(t, prop.all()[0])
			if v[0].GetVerdict() != tc.wantVerd || v[0].GetNotify() != tc.wantNotify {
				t.Errorf("verdict %q notify %v, want %q notify %v", v[0].GetVerdict(), v[0].GetNotify(), tc.wantVerd, tc.wantNotify)
			}
		})
	}
}

func TestJudgeEpisodeWithinOnePass(t *testing.T) {
	down := rjBase.Add(-70 * time.Minute)
	up := rjBase.Add(-60 * time.Minute)
	st := &rjState{
		monitors: []store.Monitor{rjMonitor("m1", store.StatusUp, rjBase.Add(-2*time.Hour))},
		// The history shows neither change: the DOWN is a false alarm. The UP is
		// in the history, but follows a false DOWN, so it is a false alarm too.
		history: map[string][]store.StatusChange{rjKey(testTenant, "m1"): {{SinceMs: rjMs(up), Status: store.StatusUp}}},
		open: []store.UnconfirmedAlert{
			rjAlert("node-a", 1, "m1", "DOWN", down, alert.LocalOutcomeSent),
			rjAlert("node-a", 2, "m1", "UP", up, alert.LocalOutcomeSent),
		},
	}
	prop := &rjProposer{}
	rjJudge(st, prop, rjBase, 0).tick(context.Background())
	v := resolvedVerdicts(t, prop.all()[0])
	if len(v) != 2 || v[0].GetVerdict() != store.VerdictFalseAlarm || v[1].GetVerdict() != store.VerdictFalseAlarm {
		t.Fatalf("verdicts %v, want false_alarm for DOWN and UP", v)
	}
}

func TestJudgeEpisodeAcrossPasses(t *testing.T) {
	down := rjBase.Add(-70 * time.Minute)
	up := rjBase.Add(-60 * time.Minute)
	st := &rjState{
		monitors: []store.Monitor{rjMonitor("m1", store.StatusUp, rjBase.Add(-2*time.Hour))},
		history:  map[string][]store.StatusChange{rjKey(testTenant, "m1"): {{SinceMs: rjMs(up), Status: store.StatusUp}}},
		open:     []store.UnconfirmedAlert{rjAlert("node-a", 1, "m1", "DOWN", down, alert.LocalOutcomeSent)},
	}
	prop := &rjProposer{}
	j := rjJudge(st, prop, rjBase, 0)
	j.tick(context.Background())

	// The DOWN is resolved; only the UP is open in the next pass.
	st.mu.Lock()
	st.open = []store.UnconfirmedAlert{rjAlert("node-a", 2, "m1", "UP", up, alert.LocalOutcomeSent)}
	st.mu.Unlock()
	j.tick(context.Background())

	cmds := prop.all()
	if len(cmds) != 2 {
		t.Fatalf("%d commands, want 2", len(cmds))
	}
	if v := resolvedVerdicts(t, cmds[1]); v[0].GetVerdict() != store.VerdictFalseAlarm {
		t.Errorf("UP of a false DOWN judged in a later pass: verdict %q, want false_alarm", v[0].GetVerdict())
	}
}

// TestJudgeEpisodeWaitsForWindows checks that an episode is judged in order:
// neither alert is decided while its window is open, and both are decided in
// one later pass, the DOWN first, so the UP follows the false DOWN.
func TestJudgeEpisodeWaitsForWindows(t *testing.T) {
	down := rjBase.Add(-3 * time.Minute) // window ends at now: not settled
	up := rjBase.Add(-2 * time.Minute)
	st := &rjState{
		monitors: []store.Monitor{rjMonitor("m1", store.StatusUp, rjBase.Add(-2*time.Hour))},
		history:  map[string][]store.StatusChange{rjKey(testTenant, "m1"): {{SinceMs: rjMs(up), Status: store.StatusUp}}},
		open: []store.UnconfirmedAlert{
			rjAlert("node-a", 1, "m1", "DOWN", down, alert.LocalOutcomeSent),
			rjAlert("node-a", 2, "m1", "UP", up, alert.LocalOutcomeSent),
		},
	}
	prop := &rjProposer{}
	j := rjJudge(st, prop, rjBase, 0)
	j.tick(context.Background())
	if n := len(prop.all()); n != 0 {
		t.Fatalf("%d commands while the windows are open, want none", n)
	}

	j.opts.Now = func() time.Time { return rjBase.Add(2 * time.Minute) }
	j.tick(context.Background())
	cmds := prop.all()
	if len(cmds) != 1 {
		t.Fatalf("%d commands after the windows settled, want 1", len(cmds))
	}
	v := resolvedVerdicts(t, cmds[0])
	if len(v) != 2 || v[0].GetLocalId() != 1 || v[0].GetVerdict() != store.VerdictFalseAlarm || v[1].GetVerdict() != store.VerdictFalseAlarm {
		t.Errorf("verdicts %v, want the DOWN then the UP, both false_alarm", v)
	}
}

func TestJudgeUnverifiable(t *testing.T) {
	since := rjBase.Add(-time.Hour)
	st := &rjState{
		monitors: []store.Monitor{rjMonitor("m1", store.StatusUp, rjBase.Add(-2*time.Hour))},
		open: []store.UnconfirmedAlert{
			rjAlert("node-a", 1, "gone", "DOWN", since, alert.LocalOutcomeSent),
			rjAlert("node-a", 2, "m1", "DOWN", rjBase.Add(-48*time.Hour), alert.LocalOutcomeSent),
		},
		history: map[string][]store.StatusChange{rjKey(testTenant, "m1"): {{SinceMs: rjMs(since), Status: store.StatusDown}}},
	}
	prop := &rjProposer{}
	// Raw data kept 24 h: the alert of two days ago is past retention.
	rjJudge(st, prop, rjBase, 24*time.Hour).tick(context.Background())
	v := resolvedVerdicts(t, prop.all()[0])
	for i, verdict := range v {
		if verdict.GetVerdict() != store.VerdictUnverifiable || verdict.GetNotify() {
			t.Errorf("verdict %d: %q notify %v, want unverifiable without notification", i, verdict.GetVerdict(), verdict.GetNotify())
		}
	}
}

func TestJudgeGates(t *testing.T) {
	since := rjBase.Add(-time.Hour)
	st := &rjState{
		monitors: []store.Monitor{rjMonitor("m1", store.StatusUp, rjBase.Add(-2*time.Hour))},
		history:  map[string][]store.StatusChange{rjKey(testTenant, "m1"): {{SinceMs: rjMs(since), Status: store.StatusDown}}},
		open:     []store.UnconfirmedAlert{rjAlert("node-a", 1, "m1", "DOWN", since, alert.LocalOutcomeSent)},
	}
	leader := true
	gapDone := false
	level := uint32(cluster.DegradedCommandLevel)
	prop := &rjProposer{}
	j := NewJudge(JudgeOptions{
		State: st, Proposer: prop, IsLeader: func() bool { return leader },
		CommandLevel: func(context.Context) (uint32, error) { return level, nil },
		GapDone:      func() bool { return gapDone }, Now: func() time.Time { return rjBase }, Logger: rjLogger(),
	})
	ctx := context.Background()

	leader = false
	j.tick(ctx)
	leader, level = true, 4
	j.tick(ctx)
	level = 5
	j.tick(ctx)
	if n := len(prop.all()); n != 0 {
		t.Fatalf("%d commands while not leader, below level 5 or before the gap job, want none", n)
	}
	gapDone = true
	j.tick(ctx)
	if n := len(prop.all()); n != 1 {
		t.Errorf("%d commands once all gates pass, want 1", n)
	}
}

// ---------------------------------------------------------------- GapJob

// rjGap builds a gap job with a fixed clock and the given state, re-evaluator and peers.
type rjGapRig struct {
	st     *rjState
	reeval *rjReeval
	prop   *rjProposer
	leader *bool
	job    *GapJob
	peers  []replication.PeerStatus
}

func newRjGapRig(t *testing.T, monitors ...store.Monitor) *rjGapRig {
	t.Helper()
	leader := false
	rig := &rjGapRig{
		st:     &rjState{tenants: []store.Tenant{{ID: testTenant}}, monitors: monitors, lastIssued: rjMs(rjBase.Add(-2 * time.Hour))},
		reeval: &rjReeval{changes: map[string][]aggregator.Change{}},
		prop:   &rjProposer{},
		leader: &leader,
	}
	rig.job = NewGapJob(GapOptions{
		NodeID: testNode, State: rig.st, Reevaluator: rig.reeval, Proposer: rig.prop,
		IsLeader:     func() bool { return *rig.leader },
		CommandLevel: rjCmdLevel(cluster.DegradedCommandLevel),
		Replication:  func() []replication.PeerStatus { return rig.peers },
		Tick:         time.Millisecond, CatchUpWait: 20 * time.Millisecond,
		Now: func() time.Time { return rjBase }, Logger: rjLogger(),
	})
	// Run samples once at its start; the tests start from that sample.
	rig.job.sample(context.Background())
	return rig
}

// lead makes the node leader and runs one tick of the job.
func (rig *rjGapRig) lead(t *testing.T) {
	t.Helper()
	*rig.leader = true
	rig.job.tick(context.Background())
}

func TestGapJobNoGapIsDoneAtOnce(t *testing.T) {
	// The last command is 4 min old, so the gap ends before the live window.
	rig := newRjGapRig(t, rjMonitor("m1", store.StatusUp, rjBase.Add(-10*time.Hour)))
	rig.st.lastIssued = rjMs(rjBase.Add(-4 * time.Minute))
	rig.job.sample(context.Background())
	rig.lead(t)
	if !rig.job.Done() {
		t.Error("Done() = false, want true: no gap")
	}
	if n := len(rig.prop.all()); n != 0 {
		t.Errorf("%d commands, want none", n)
	}
	if len(rig.reeval.requests) != 0 {
		t.Errorf("re-evaluated %d gaps, want none", len(rig.reeval.requests))
	}
}

func TestGapJobRecordsHistoryAndCurrentChange(t *testing.T) {
	rig := newRjGapRig(t, rjMonitor("m1", store.StatusUp, rjBase.Add(-10*time.Hour)))
	down := rjBase.Add(-100 * time.Minute)
	up := rjBase.Add(-50 * time.Minute)
	rig.reeval.changes["m1"] = []aggregator.Change{
		{SinceMs: rjMs(down), Status: store.StatusDown, Incident: &store.Incident{ID: "inc-1", Cause: "timeout", ErrorClass: "timeout"}, Reporters: []string{"node-a"}},
		{SinceMs: rjMs(up), Status: store.StatusUp, Reporters: []string{"node-a", "node-b"}},
	}
	rig.lead(t)

	if !rig.job.Done() {
		t.Fatal("Done() = false, want true after the gap")
	}
	// The gap starts at the last command of the follower, the live window at now - 5 min.
	if len(rig.reeval.requests) != 1 {
		t.Fatalf("%d gap requests, want 1", len(rig.reeval.requests))
	}
	req := rig.reeval.requests[0]
	if req.from != store.StatusUp || req.fromMs != rjMs(rjBase.Add(-2*time.Hour)) || req.toMs != rjMs(rjBase.Add(-5*time.Minute)) {
		t.Errorf("gap request %+v, want from UP [now-2h, now-5min)", req)
	}

	cmds := rig.prop.all()
	if len(cmds) != 2 {
		t.Fatalf("%d commands, want history and state", len(cmds))
	}
	hist := cmds[0].GetRecordMonitorHistory()
	if hist == nil {
		t.Fatalf("first command %T, want RecordMonitorHistory", cmds[0].GetPayload())
	}
	if hist.GetExpectedStatus() != "UP" || hist.GetFromMs() != req.fromMs || hist.GetToMs() != rjMs(up) {
		t.Errorf("history expected %q [%d, %d), want UP [%d, %d)", hist.GetExpectedStatus(), hist.GetFromMs(), hist.GetToMs(), req.fromMs, rjMs(up))
	}
	if len(hist.GetEntries()) != 1 || hist.GetEntries()[0].GetSinceMs() != rjMs(down) || hist.GetEntries()[0].GetIncidentId() != "inc-1" {
		t.Errorf("history entries %v, want the DOWN at %d with incident inc-1", hist.GetEntries(), rjMs(down))
	}

	set := cmds[1].GetSetMonitorState()
	if set == nil {
		t.Fatalf("second command %T, want SetMonitorState", cmds[1].GetPayload())
	}
	if set.GetExpectedStatus() != "DOWN" || set.GetStatus() != "UP" || set.GetSinceMs() != rjMs(up) {
		t.Errorf("state expected %q to %q since %d, want DOWN to UP since %d", set.GetExpectedStatus(), set.GetStatus(), set.GetSinceMs(), rjMs(up))
	}
	if set.GetSenderNodeId() != testNode || !set.GetConfirmed() || !slices.Equal(set.GetReporters(), []string{"node-a", "node-b"}) {
		t.Errorf("state sender %q confirmed %v reporters %v, want %s, true, [node-a node-b]", set.GetSenderNodeId(), set.GetConfirmed(), set.GetReporters(), testNode)
	}
}

func TestGapJobOngoingOutageIsSetWithQuorum(t *testing.T) {
	rig := newRjGapRig(t, rjMonitor("m1", store.StatusUp, rjBase.Add(-10*time.Hour)))
	down := rjBase.Add(-20 * time.Minute)
	rig.reeval.changes["m1"] = []aggregator.Change{
		{SinceMs: rjMs(down), Status: store.StatusDown, Incident: &store.Incident{ID: "inc-2", Cause: "dns"}},
	}
	rig.lead(t)
	cmds := rig.prop.all()
	if len(cmds) != 1 || cmds[0].GetSetMonitorState() == nil {
		t.Fatalf("commands %v, want one SetMonitorState only", cmds)
	}
	set := cmds[0].GetSetMonitorState()
	if set.GetExpectedStatus() != "UP" || set.GetStatus() != "DOWN" || set.GetSinceMs() != rjMs(down) || set.GetIncident().GetIncidentId() != "inc-2" {
		t.Errorf("state %+v, want UP to DOWN since %d with incident inc-2", set, rjMs(down))
	}
}

func TestGapJobConflictContinuesWithNextMonitor(t *testing.T) {
	rig := newRjGapRig(t,
		rjMonitor("m1", store.StatusUp, rjBase.Add(-10*time.Hour)),
		rjMonitor("m2", store.StatusUp, rjBase.Add(-10*time.Hour)),
	)
	down := rjBase.Add(-20 * time.Minute)
	change := []aggregator.Change{{SinceMs: rjMs(down), Status: store.StatusDown, Incident: &store.Incident{ID: "inc"}}}
	rig.reeval.changes["m1"] = change
	rig.reeval.changes["m2"] = change
	rig.prop.fail = func(cmd *pb.Command) error {
		if cmd.GetSetMonitorState().GetMonitorId() == "m1" {
			return &cluster.CommandError{Code: cluster.CodeConflict, Message: "changed meanwhile"}
		}
		return nil
	}
	rig.lead(t)
	if !rig.job.Done() {
		t.Error("Done() = false, want true after a conflict")
	}
	if n := len(rig.prop.all()); n != 2 {
		t.Errorf("%d commands, want one per monitor (the second monitor still runs)", n)
	}
}

func TestGapJobOtherErrorStaysOpen(t *testing.T) {
	rig := newRjGapRig(t,
		rjMonitor("m1", store.StatusUp, rjBase.Add(-10*time.Hour)),
		rjMonitor("m2", store.StatusUp, rjBase.Add(-10*time.Hour)),
	)
	down := rjBase.Add(-20 * time.Minute)
	change := []aggregator.Change{{SinceMs: rjMs(down), Status: store.StatusDown, Incident: &store.Incident{ID: "inc"}}}
	rig.reeval.changes["m1"] = change
	rig.reeval.changes["m2"] = change
	rig.prop.fail = func(*pb.Command) error { return errors.New("not leader") }
	rig.lead(t)
	if rig.job.Done() {
		t.Error("Done() = true after a failed command, want false so that the next tick retries")
	}
	if n := len(rig.prop.all()); n != 1 {
		t.Errorf("%d commands, want the job to stop at the failed monitor", n)
	}
}

func TestGapJobBelowLevelFiveIsDoneWithoutCommands(t *testing.T) {
	rig := newRjGapRig(t, rjMonitor("m1", store.StatusUp, rjBase.Add(-10*time.Hour)))
	rig.job.opts.CommandLevel = rjCmdLevel(4)
	rig.reeval.changes["m1"] = []aggregator.Change{{SinceMs: rjMs(rjBase.Add(-20 * time.Minute)), Status: store.StatusDown, Incident: &store.Incident{ID: "x"}}}
	rig.lead(t)
	if !rig.job.Done() {
		t.Error("Done() = false, want true below level 5")
	}
	if n := len(rig.prop.all()) + len(rig.reeval.requests); n != 0 {
		t.Errorf("%d commands and gap requests below level 5, want none", n)
	}
}

func TestGapJobDoneResetsAfterLeadershipLoss(t *testing.T) {
	rig := newRjGapRig(t, rjMonitor("m1", store.StatusUp, rjBase.Add(-10*time.Hour)))
	rig.lead(t)
	if !rig.job.Done() {
		t.Fatal("Done() = false after the first leadership")
	}
	*rig.leader = false
	rig.job.tick(context.Background())
	if rig.job.Done() {
		t.Error("Done() = true after losing leadership, want false")
	}
	rig.lead(t)
	if !rig.job.Done() {
		t.Error("Done() = false after the second leadership, want true")
	}
}

func TestGapJobWaitsForCatchUpWithinBound(t *testing.T) {
	rig := newRjGapRig(t, rjMonitor("m1", store.StatusUp, rjBase.Add(-10*time.Hour)))
	// A connected peer that never completed a round after the leadership start.
	rig.peers = []replication.PeerStatus{{ID: "node-b", Connected: true, CaughtUpAt: rjBase.Add(-time.Hour)}}
	rig.reeval.changes["m1"] = []aggregator.Change{{SinceMs: rjMs(rjBase.Add(-20 * time.Minute)), Status: store.StatusDown, Incident: &store.Incident{ID: "x"}}}
	rig.lead(t)
	// The wait times out and the gap is re-evaluated anyway.
	if !rig.job.Done() || len(rig.reeval.requests) != 1 {
		t.Errorf("done %v, gap requests %d; want done after the bounded wait", rig.job.Done(), len(rig.reeval.requests))
	}
}

func TestGapJobSamplesWhileNotLeader(t *testing.T) {
	rig := newRjGapRig(t, rjMonitor("m1", store.StatusUp, rjBase.Add(-10*time.Hour)))
	rig.job.tick(context.Background())
	rig.st.mu.Lock()
	rig.st.lastIssued = rjMs(rjBase.Add(-30 * time.Minute))
	rig.st.mu.Unlock()
	rig.job.tick(context.Background())
	if rig.job.lastSeen != rjMs(rjBase.Add(-30*time.Minute)) {
		t.Fatalf("lastSeen %d, want the sampled command time", rig.job.lastSeen)
	}
	// While leader, the sample is not updated.
	rig.st.mu.Lock()
	rig.st.lastIssued = rjMs(rjBase)
	rig.st.mu.Unlock()
	rig.lead(t)
	if rig.job.lastSeen != rjMs(rjBase.Add(-30*time.Minute)) {
		t.Errorf("lastSeen %d changed while leader", rig.job.lastSeen)
	}
}

func TestCaughtUp(t *testing.T) {
	since := rjBase
	cases := []struct {
		name  string
		peers []replication.PeerStatus
		want  bool
	}{
		{name: "no peers", want: true},
		{name: "disconnected peer is ignored", peers: []replication.PeerStatus{{ID: "b"}}, want: true},
		{name: "connected and caught up after start", peers: []replication.PeerStatus{{ID: "b", Connected: true, CaughtUpAt: since.Add(time.Second)}}, want: true},
		{name: "connected, round before start", peers: []replication.PeerStatus{{ID: "b", Connected: true, CaughtUpAt: since.Add(-time.Second)}}},
		{name: "connected, never caught up", peers: []replication.PeerStatus{{ID: "b", Connected: true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := caughtUp(tc.peers, since); got != tc.want {
				t.Errorf("caughtUp = %v, want %v", got, tc.want)
			}
		})
	}
}

// rjLogger keeps the test output quiet.
func rjLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
