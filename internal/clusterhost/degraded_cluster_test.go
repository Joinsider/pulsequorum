// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package clusterhost

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/aggregator"
	"github.com/joinsider/pulsequorum/internal/alert"
	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/degraded"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/scheduler"
	"github.com/joinsider/pulsequorum/internal/store"
	_ "modernc.org/sqlite" // driver "sqlite" for the read-only check of unconfirmed_alerts
)

// ADR 0024, FA-ALR-10/11, FA-UPT-07, NFA-REL-06 and AS-04: when the quorum is
// lost, the reachable voter with the smallest ID sends the alerts itself and
// marks them unconfirmed. After the quorum returned, the alerts are reported
// to the leader, judged against the confirmed history, and the FSM of all
// voters is identical again.
//
// Every voter runs the same components as cmd/pulsequorum: the live
// aggregator (gated by the gap job), the degraded detector, overlay, local
// dispatcher, applier, degraded aggregator, reporter, judge, gap job and the
// alert dispatcher of the leader. The heartbeats are synthetic (one per slot of
// the monitor, as in TestConsensusAcrossVoters); the replicated heartbeats reach
// the aggregators through the OnHeartbeatsStored hook, as in the binary.

const (
	degMonitorID = "degraded-web"
	degChannelID = "hook"
	// degAfter is DEGRADED_AFTER of the test (the default is 15 s).
	degAfter = 1500 * time.Millisecond
	// degTick is the period of the degraded components and the judge (the defaults are 1 s to 30 s).
	degTick = 500 * time.Millisecond
	// degVerdict bounds the wait for the verdict: the judge decides a window
	// once it is 30 s old (judgeSettle), so the wait includes that settle time.
	degVerdict = 90 * time.Second
)

// degNode holds the degraded components of one voter.
type degNode struct {
	cn       *consensusNode
	detector *degraded.Detector
	degAgg   atomic.Pointer[aggregator.Aggregator]
}

// observe hands stored heartbeats to the live and the degraded aggregator.
func (dn *degNode) observe(hbs []store.Heartbeat) {
	if a := dn.cn.agg.Load(); a != nil {
		a.Observe(hbs)
	}
	if a := dn.degAgg.Load(); a != nil {
		a.Observe(hbs)
	}
}

// degCluster is a consensus cluster whose voters run the degraded stack.
type degCluster struct {
	*consensusCluster
	byID map[string]*degNode
}

func newDegCluster(ctx context.Context, t *testing.T) *degCluster {
	t.Helper()
	return &degCluster{consensusCluster: &consensusCluster{t: t, ctx: ctx}, byID: map[string]*degNode{}}
}

// startVoter launches a voter whose replicated heartbeats feed its aggregators.
func (d *degCluster) startVoter(seeds []string, token string) *consensusNode {
	d.t.Helper()
	cn := &consensusNode{}
	dn := &degNode{cn: cn}
	n, err := launch(d.ctx, d.t, &testNode{id: uuid.NewString(), addr: freeAddr(d.t), dir: d.t.TempDir(),
		replInterval: 300 * time.Millisecond, onStored: dn.observe}, seeds, token)
	if err != nil {
		d.t.Fatal(err)
	}
	cn.testNode = n
	d.nodes = append(d.nodes, cn)
	d.byID[cn.id] = dn
	return cn
}

// startVoters starts three voters, n2 through n1 and n3 through n2 (FA-CLU-03).
func (d *degCluster) startVoters() []*consensusNode {
	d.t.Helper()
	n1 := d.startVoter(nil, "")
	eventually(d.t, "n1 leader", 10*time.Second, n1.host.Node.IsLeader)
	token := func() string {
		tok, _, err := n1.host.Authority.CreateJoinToken(d.ctx, pki.RoleVoter, "", 0)
		if err != nil {
			d.t.Fatal(err)
		}
		return tok
	}
	n2 := d.startVoter([]string{n1.addr}, token())
	eventually(d.t, "n2 promoted to voter", 30*time.Second, func() bool { return isVoter(n1.testNode, n2.id) })
	n3 := d.startVoter([]string{n2.addr}, token())
	eventually(d.t, "n3 promoted to voter", 30*time.Second, func() bool { return isVoter(n1.testNode, n3.id) })
	eventually(d.t, "registry lists three voters", 30*time.Second, func() bool { return memberCount(n1.testNode) == 3 })
	return d.nodes
}

// setup creates the webhook channel and the test monitor through the leader.
func (d *degCluster) setup(webhookURL string) {
	d.t.Helper()
	leader := d.leader()
	enc, err := alert.EncryptURL(leader.keyring, degChannelID, webhookURL)
	if err != nil {
		d.t.Fatal(err)
	}
	ch, err := cluster.NewPutChannel("", cluster.DefaultTenantID, degChannelID, cluster.ChannelDocument{
		Name: degChannelID, Kind: store.ChannelWebhook, URLEnc: enc, Events: []string{store.EventDown, store.EventUp},
	}, 0)
	if err != nil {
		d.t.Fatal(err)
	}
	doc := cluster.MonitorDocument{Name: degMonitorID, Type: "http", IntervalSeconds: 1, TimeoutSeconds: 1,
		Retries: 0, RecoveryThreshold: 1, Assignment: store.Assignment{Mode: "all"}, ConsensusPolicy: "majority",
		NotificationChannelIDs: []string{degChannelID}}
	mon, err := cluster.NewPutMonitor("", cluster.DefaultTenantID, degMonitorID, doc, 0)
	if err != nil {
		d.t.Fatal(err)
	}
	for _, cmd := range []*pb.Command{ch, mon} {
		if _, err := leader.host.Node.Apply(d.ctx, cmd); err != nil {
			d.t.Fatal(err)
		}
	}
}

// startStack runs the aggregators, the degraded components and the feed of
// one voter, wired as cmd/pulsequorum does. The monitor must exist.
func (d *degCluster) startStack(dn *degNode) {
	d.t.Helper()
	cn := dn.cn
	host := cn.host
	ctx, cancel := context.WithCancel(d.ctx)
	cn.halt = cancel
	discard := slog.New(slog.DiscardHandler)
	assigner := scheduler.Assigner{Capability: scheduler.Capabilities(checker.Default)}
	registry := func(ctx context.Context) ([]store.Node, error) { return cn.state.ListNodes(ctx) }

	// The live aggregator evaluates only on the leader, after its gap job.
	var gap *degraded.GapJob
	agg := aggregator.New(aggregator.Options{
		State: cn.state, Metrics: cn.metrics, Cluster: host.Node, NodeID: cn.id,
		IsLeader: func() bool { return host.Node.IsLeader() && gap != nil && gap.Done() },
		Registry: registry, Assigner: assigner,
		LeaderGrace: consensusGrace, Unit: consensusUnit, Logger: discard,
	})
	cn.agg.Store(agg)

	detector := degraded.New(degraded.Options{
		NodeID: cn.id, LeaderID: host.LeaderID, CaughtUp: host.CaughtUp,
		Voters: func(ctx context.Context) ([]string, error) {
			nodes, err := cn.state.ListNodes(ctx)
			if err != nil {
				return nil, err
			}
			var voters []string
			for _, n := range nodes {
				if n.Role == store.NodeRoleVoter {
					voters = append(voters, n.ID)
				}
			}
			return voters, nil
		},
		Peers: host.PeerViews, SelfCheckFailed: func() bool { return false },
		After: degAfter, Interval: degTick, Logger: discard,
	})
	dn.detector = detector

	overlay := degraded.NewOverlay(cn.state)
	sender := alert.NewSender(&checker.Dialer{}, discard)
	local := alert.NewLocalDispatcher(alert.LocalDispatcherOptions{Sender: sender, Interval: degTick, Logger: discard})
	applier := degraded.NewApplier(degraded.ApplierOptions{
		NodeID: cn.id, State: cn.state, Overlay: overlay, Metrics: cn.metrics, Detector: detector.State,
		SelfCheckFailed: func() bool { return false }, Keyring: cn.keyring, Dispatcher: local, Logger: discard,
	})
	degAgg := aggregator.New(aggregator.Options{
		State: overlay, Metrics: cn.metrics, Cluster: applier, NodeID: cn.id,
		IsLeader: detector.IsSender, Registry: registry, Assigner: assigner,
		LeaderGrace: consensusGrace, Unit: consensusUnit, Logger: discard,
	})
	dn.degAgg.Store(degAgg)

	reporter := degraded.NewReporter(degraded.ReporterOptions{
		NodeID: cn.id, Detector: detector.State, LeaderID: host.LeaderID, CaughtUp: host.CaughtUp,
		CommandLevel: host.Node.CommandLevel, Proposer: host.Node, Metrics: cn.metrics, Interval: degTick, Logger: discard,
	})
	gap = degraded.NewGapJob(degraded.GapOptions{
		NodeID: cn.id, State: cn.state, Reevaluator: agg, Proposer: host.Node, IsLeader: host.Node.IsLeader,
		CommandLevel: host.Node.CommandLevel, Replication: host.Replication.Status, Peers: host.PeerViews, Trigger: host.Replication.Trigger,
		Unit: consensusUnit, StartLastSeen: cn.lastIssuedAtStart,
		Degraded: func() bool { return detector.State().Degraded },
		Tick:     degTick, Logger: discard,
	})
	judge := degraded.NewJudge(degraded.JudgeOptions{
		State: cn.state, Proposer: host.Node, IsLeader: host.Node.IsLeader, CommandLevel: host.Node.CommandLevel,
		GapDone: gap.Done, Unit: consensusUnit, Interval: degTick, Logger: discard,
	})
	live := alert.NewDispatcher(alert.DispatcherOptions{
		State: cn.state, Cluster: host.Node, Keyring: cn.keyring, Sender: sender,
		IsLeader: host.Node.IsLeader, Logger: discard, Interval: degTick, ReminderInterval: time.Hour,
	})
	// Leaving the sender role drops the statuses decided locally (main.go).
	resetOverlay := func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				return
			case <-detector.Changes():
				if !detector.IsSender() {
					overlay.Reset()
				}
			}
		}
	}
	loops := []func(context.Context){
		agg.Run, detector.Run, local.Run, degAgg.Run, resetOverlay,
		reporter.Run, gap.Run, judge.Run, live.Run,
		func(ctx context.Context) { d.feed(ctx, dn) },
	}
	cn.running.Add(len(loops))
	for _, loop := range loops {
		go func() {
			defer cn.running.Done()
			loop(ctx)
		}()
	}
	d.syncLive(cn)
}

// syncLive gives the live aggregator the monitors, as scheduler.Reconcile does.
func (d *degCluster) syncLive(cn *consensusNode) {
	d.t.Helper()
	var m store.Monitor
	eventually(d.t, "monitor replicated to "+cn.id, 15*time.Second, func() bool {
		var err error
		m, err = cn.state.GetMonitor(d.ctx, cluster.DefaultTenantID, degMonitorID)
		return err == nil
	})
	cn.agg.Load().Sync([]store.Monitor{m})
	// The degraded aggregator gets the monitors like the live one (main.go, scheduler.Reconcile).
	if a := d.byID[cn.id].degAgg.Load(); a != nil {
		a.Sync([]store.Monitor{m})
	}
}

// feed stores one heartbeat per slot of the monitor with the status that the
// test set for the voter, and hands it to the replication and the aggregators.
func (d *degCluster) feed(ctx context.Context, dn *degNode) {
	cn := dn.cn
	phase := scheduler.Phase(degMonitorID, consensusUnit).Milliseconds()
	unit := consensusUnit.Milliseconds()
	slot := (time.Now().UnixMilli()-phase)/unit + 1
	// The sequence continues across restarts: the incarnation may be the same.
	seq := uint64(time.Now().UnixNano())
	for {
		at := phase + slot*unit + 10
		if !sleepUntil(ctx, time.UnixMilli(at)) {
			return
		}
		seq++
		batch := []store.Heartbeat{{
			NodeID: cn.id, Incarnation: cn.metrics.Incarnation(), Seq: seq,
			TenantID: cluster.DefaultTenantID, MonitorID: degMonitorID, TsMs: at,
			Status: store.CheckStatus(cn.status.Load()), Details: json.RawMessage("{}"),
		}}
		if _, err := cn.metrics.InsertHeartbeats(ctx, batch); err != nil {
			if ctx.Err() == nil {
				d.t.Errorf("storing heartbeat of %s: %v", cn.id, err)
			}
			return
		}
		cn.host.Replication.Publish(batch)
		dn.observe(batch)
		slot++
	}
}

// restart starts the stopped voter old again from its data directory under a
// new address (FA-CLU-10). Its feed reports DOWN, like the others.
func (d *degCluster) restart(old *consensusNode) *consensusNode {
	d.t.Helper()
	cn := &consensusNode{}
	dn := &degNode{cn: cn}
	// The voter keeps its cluster address (FA-CLU-10). The port of the stopped
	// listener may still be busy for a moment, so the start is retried.
	var n *testNode
	eventually(d.t, "listen on "+old.addr+" again", 30*time.Second, func() bool {
		var err error
		n, err = launch(d.ctx, d.t, &testNode{id: old.id, addr: old.addr, dir: old.dir, replInterval: old.replInterval,
			onStored: dn.observe}, nil, "")
		if err != nil {
			d.t.Logf("restart of %s on %s: %v", old.id, old.addr, err)
		}
		return err == nil
	})
	cn.testNode = n
	cn.status.Store(int32(store.CheckDown))
	for i, c := range d.nodes {
		if c.id == old.id {
			d.nodes[i] = cn
		}
	}
	d.byID[cn.id] = dn
	return cn
}

// degradedAlerts returns the alerts of the node-local log that have not been
// reported to the leader yet.
func degradedAlerts(ctx context.Context, t *testing.T, cn *consensusNode) []store.DegradedAlert {
	t.Helper()
	rows, err := cn.metrics.ListUnreportedDegradedAlerts(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// degWebhook records the JSON payloads that the webhook receives.
type degWebhook struct {
	mu       sync.Mutex
	payloads []cluster.AlertPayload
}

// newDegWebhook starts an httptest server that records the alerts.
func newDegWebhook(t *testing.T) (string, *degWebhook) {
	t.Helper()
	rec := &degWebhook{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p cluster.AlertPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.mu.Lock()
		rec.payloads = append(rec.payloads, p)
		rec.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, rec
}

// all returns a copy of the recorded payloads.
func (r *degWebhook) all() []cluster.AlertPayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]cluster.AlertPayload(nil), r.payloads...)
}

// waitFor waits until a recorded payload matches pred and returns it.
func (r *degWebhook) waitFor(t *testing.T, what string, timeout time.Duration, pred func(cluster.AlertPayload) bool) cluster.AlertPayload {
	t.Helper()
	var found cluster.AlertPayload
	eventually(t, what, timeout, func() bool {
		for _, p := range r.all() {
			if pred(p) {
				found = p
				return true
			}
		}
		return false
	})
	return found
}

// fingerprints returns the FSM fingerprint of every live voter.
func (d *degCluster) fingerprints() map[string]string {
	d.t.Helper()
	out := map[string]string{}
	for _, cn := range d.live() {
		fp, err := cn.state.Fingerprint(d.ctx)
		if err != nil {
			d.t.Fatal(err)
		}
		out[cn.id] = fp
	}
	return out
}

// unconfirmedRecord is the part of an unconfirmed_alerts row that the test checks.
type unconfirmedRecord struct {
	verdict string
}

// unconfirmedRow reads one row of unconfirmed_alerts from the state file of cn
// over a read-only connection. The state store exposes the row only inside its
// write transaction, which the FSM alone may use. ok is false if the row is missing.
func unconfirmedRow(ctx context.Context, t *testing.T, cn *consensusNode, nodeID string, localID int64) (unconfirmedRecord, bool) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+cn.state.DB().Path()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var rec unconfirmedRecord
	err = db.QueryRowContext(ctx, "SELECT verdict FROM unconfirmed_alerts WHERE node_id = ? AND local_id = ?", nodeID, localID).Scan(&rec.verdict)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return rec, true
}

// waitDegMonitor waits until every live voter shows the test monitor with status
// want. consensusCluster.waitStatus is bound to consensusMonitorID, so it is not used.
func (d *degCluster) waitDegMonitor(want store.MonitorStatus, timeout time.Duration) {
	d.t.Helper()
	for _, cn := range d.live() {
		eventually(d.t, fmt.Sprintf("status %s of %s on %s", want, degMonitorID, cn.id), timeout, func() bool {
			m, err := cn.state.GetMonitor(d.ctx, cluster.DefaultTenantID, degMonitorID)
			return err == nil && m.State.Status == want
		})
	}
}

// TestDegradedModeAlertsAndCorrects: two of three voters fail, the remaining
// voter alerts unconfirmed; after they return, the alert is reported, judged and
// the FSM converges.
func TestDegradedModeAlertsAndCorrects(t *testing.T) {
	if testing.Short() {
		t.Skip("starts three voters and runs the degraded mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	d := newDegCluster(ctx, t)
	defer d.shutdown()

	voters := d.startVoters()
	eventually(t, "command level >= 5", 30*time.Second, func() bool {
		level, err := voters[0].host.Node.CommandLevel(ctx)
		return err == nil && level >= cluster.DegradedCommandLevel
	})
	webhookURL, hook := newDegWebhook(t)
	d.setup(webhookURL)
	for _, cn := range d.live() {
		d.startStack(d.byID[cn.id])
	}

	// Step 2: every voter reports UP.
	d.report(store.CheckUp)
	d.waitDegMonitor(store.StatusUp, 30*time.Second)

	// Step 3: two voters stop, the remaining one loses the quorum and reports DOWN.
	remaining := d.live()[0]
	stopped := append([]*consensusNode(nil), d.live()[1:]...)
	for _, cn := range stopped {
		d.stop(cn)
	}
	d.report(store.CheckDown)
	rd := d.byID[remaining.id]

	// Step 4: the remaining voter is degraded and sends the DOWN alert unconfirmed.
	eventually(t, "remaining voter degraded and sender", 30*time.Second, func() bool {
		st := rd.detector.State()
		return st.Degraded && st.Sender == remaining.id
	})
	hook.waitFor(t, "unconfirmed DOWN alert", 60*time.Second, func(p cluster.AlertPayload) bool {
		return p.Status == string(store.StatusDown) && !p.Confirmed && p.Sender != nil &&
			p.Sender.ReachableVoters == 1 && p.Sender.TotalVoters == 3
	})
	var localID int64
	var sinceOfAlert int64
	for _, row := range degradedAlerts(ctx, t, remaining) {
		if row.MonitorID == degMonitorID && row.Status == string(store.StatusDown) {
			localID = row.ID
			sinceOfAlert = row.SinceMs
		}
	}
	if localID == 0 {
		t.Fatal("no DOWN row in the degraded alert log of the remaining voter")
	}

	// Step 5: the stopped voters return with their data; all feeds report DOWN.
	for _, cn := range stopped {
		restarted := d.restart(cn)
		d.startStack(d.byID[restarted.id])
	}
	d.waitDegMonitor(store.StatusDown, 60*time.Second)
	d.leader()

	eventually(t, "degraded alert reported to the leader", 60*time.Second, func() bool {
		for _, row := range degradedAlerts(ctx, t, remaining) {
			if row.ID == localID {
				return false
			}
		}
		return true
	})
	for _, cn := range d.live() {
		eventually(t, "unconfirmed alert stored on "+cn.id, 30*time.Second, func() bool {
			_, ok := unconfirmedRow(ctx, t, cn, remaining.id, localID)
			return ok
		})
	}
	for _, cn := range d.live() {
		eventually(t, "verdict stored on "+cn.id, degVerdict, func() bool {
			row, ok := unconfirmedRow(ctx, t, cn, remaining.id, localID)
			return ok && row.verdict != ""
		})
		row, _ := unconfirmedRow(ctx, t, cn, remaining.id, localID)
		if row.verdict != store.VerdictConfirmed {
			t.Errorf("verdict on %s = %q, want %q", cn.id, row.verdict, store.VerdictConfirmed)
			dumpHistory(ctx, t, cn, sinceOfAlert)
		}
	}
	eventually(t, "FSM fingerprints equal", 30*time.Second, func() bool {
		fps := d.fingerprints()
		var first string
		for _, fp := range fps {
			if first == "" {
				first = fp
			}
			if fp != first {
				return false
			}
		}
		return first != ""
	})
}

// dumpHistory logs the status history around a DOWN alert whose verdict is not
// confirmed, to show why the judge decided as it did.
func dumpHistory(ctx context.Context, t *testing.T, cn *consensusNode, sinceMs int64) {
	t.Helper()
	changes, err := cn.state.StatusHistory(ctx, cluster.DefaultTenantID, degMonitorID, sinceMs-60_000, sinceMs+60_000)
	if err != nil {
		t.Logf("status history on %s: %v", cn.id, err)
		return
	}
	if len(changes) == 0 {
		t.Logf("status history on %s: no change within 60 s of since=%d", cn.id, sinceMs)
	}
	for _, c := range changes {
		t.Logf("status history on %s: status=%s since=%d", cn.id, c.Status, c.SinceMs)
	}
}
