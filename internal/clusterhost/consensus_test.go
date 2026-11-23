// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package clusterhost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/aggregator"
	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/scheduler"
	"github.com/joinsider/pulsequorum/internal/store"
)

// FA-CON-01/02/06, ADR 0021: the consensus of a monitor is evaluated on the
// leader only, from the heartbeats of all responsible voters, and each status
// change is proposed once through Raft.
//
// Every voter runs an aggregator as cmd/pulsequorum does. The heartbeats are
// synthetic: each voter stores one heartbeat per slot of its own stream and
// hands it to the same hooks as the HeartbeatWriter (replication push and the
// aggregator), so the heartbeats reach the other voters through the real
// replication path. The writer's one-second batching is skipped, because a
// slot of the test monitor is only 500 ms long.

const (
	consensusMonitorID = "api"
	consensusUnit      = 500 * time.Millisecond // one interval second of the test monitor
	consensusGrace     = 500 * time.Millisecond // LeaderGrace of the aggregators
	consensusSettle    = 3 * consensusUnit      // time in which nothing else may happen
	// consensusPromotePoll is the production default of membership.Manager.PromotePoll (2 s).
	consensusPromotePoll = 2 * time.Second
)

// consensusNode is a voter with its aggregator, heartbeat feed and counters.
type consensusNode struct {
	*testNode
	agg     atomic.Pointer[aggregator.Aggregator]
	status  atomic.Int32 // store.CheckStatus reported by the feed
	stopped bool
	halt    context.CancelFunc // stops the feed and the aggregator
	running sync.WaitGroup     // feed and aggregator goroutines

	// proposals and followerProposals count successful status changes proposed
	// while this voter was or was not leader.
	proposals         atomic.Int32
	followerProposals atomic.Int32
	// downEvents, upEvents and incidentsOpened count the committed changes of
	// this voter's FSM for the test monitor.
	downEvents      atomic.Int32
	upEvents        atomic.Int32
	incidentsOpened atomic.Int32
}

// observeStored hands stored heartbeats to the aggregator once it exists.
func (cn *consensusNode) observeStored(hbs []store.Heartbeat) {
	if a := cn.agg.Load(); a != nil {
		a.Observe(hbs)
	}
}

// countEvent is the FSM observer of the voter.
func (cn *consensusNode) countEvent(ev cluster.StateEvent) {
	if ev.TenantID != cluster.DefaultTenantID || ev.MonitorID != consensusMonitorID {
		return
	}
	switch ev.Status {
	case store.StatusDown:
		cn.downEvents.Add(1)
	case store.StatusUp:
		cn.upEvents.Add(1)
	}
	if ev.IncidentOpened != "" {
		cn.incidentsOpened.Add(1)
	}
}

func (cn *consensusNode) resetCounters() {
	cn.proposals.Store(0)
	cn.followerProposals.Store(0)
	cn.downEvents.Store(0)
	cn.upEvents.Store(0)
	cn.incidentsOpened.Store(0)
}

// countingApplier is the Cluster of the aggregator: it proposes through the
// local Raft node and records who proposed.
type countingApplier struct{ cn *consensusNode }

func (a countingApplier) Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error) {
	leader := a.cn.host.Node.IsLeader()
	res, err := a.cn.host.Node.Apply(ctx, cmd)
	if err == nil {
		if leader {
			a.cn.proposals.Add(1)
		} else {
			a.cn.followerProposals.Add(1)
		}
	}
	return res, err
}

// consensusCluster is three voters that run the aggregator and a feed each.
type consensusCluster struct {
	t     *testing.T
	ctx   context.Context
	nodes []*consensusNode
}

// startConsensusCluster starts three voters over mTLS, like TestHeartbeatReplication.
func startConsensusCluster(ctx context.Context, t *testing.T) *consensusCluster {
	t.Helper()
	c := &consensusCluster{t: t, ctx: ctx}
	n1 := c.start(nil, "")
	eventually(t, "n1 leader", 10*time.Second, n1.host.Node.IsLeader)
	token := func() string {
		tok, _, err := n1.host.Authority.CreateJoinToken(ctx, pki.RoleVoter, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	n2 := c.start([]string{n1.addr}, token())
	eventually(t, "n2 promoted to voter", 30*time.Second, func() bool { return isVoter(n1.testNode, n2.id) })
	n3 := c.start([]string{n2.addr}, token())
	eventually(t, "n3 promoted to voter", 30*time.Second, func() bool { return isVoter(n1.testNode, n3.id) })
	for _, cn := range c.nodes {
		eventually(t, "registry lists three voters", 30*time.Second, func() bool { return len(registry(t, cn.testNode)) == 3 })
	}
	return c
}

// start launches a voter whose replication hook feeds its aggregator.
func (c *consensusCluster) start(seeds []string, token string) *consensusNode {
	c.t.Helper()
	cn := &consensusNode{}
	// The consensus scenarios rely on the production promotion period: a
	// faster promotion changes the membership while the scenario starts
	// (reproduced: DOWN changes and incidents differ from the expected ones).
	n, err := launch(c.ctx, c.t, &testNode{id: uuid.NewString(), addr: freeAddr(c.t), dir: c.t.TempDir(),
		replInterval: 300 * time.Millisecond, promotePoll: consensusPromotePoll, onStored: cn.observeStored}, seeds, token)
	if err != nil {
		c.t.Fatal(err)
	}
	cn.testNode = n
	n.host.Node.FSM().OnStateChange(cn.countEvent)
	c.nodes = append(c.nodes, cn)
	return cn
}

// setup creates the monitor (all voters responsible, majority, no retries) and
// starts the aggregator and the feed of every voter. statuses holds the
// reported status per voter, in the order of c.nodes.
func (c *consensusCluster) setup(statuses ...store.CheckStatus) {
	c.t.Helper()
	doc := cluster.MonitorDocument{Name: consensusMonitorID, Type: "http", IntervalSeconds: 1, TimeoutSeconds: 1,
		Retries: 0, RecoveryThreshold: 1, Assignment: store.Assignment{Mode: "all"}, ConsensusPolicy: "majority"}
	cmd, err := cluster.NewPutMonitor("", cluster.DefaultTenantID, consensusMonitorID, doc, 0)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.nodes[0].host.Node.Apply(c.ctx, cmd); err != nil {
		c.t.Fatal(err)
	}
	for i, cn := range c.nodes {
		cn.status.Store(statusWord(statuses[i]))
		c.startAggregator(cn)
	}
	for _, cn := range c.nodes {
		eventually(c.t, "monitor replicated to every voter", 15*time.Second, func() bool {
			_, err := cn.state.GetMonitor(c.ctx, cluster.DefaultTenantID, consensusMonitorID)
			return err == nil
		})
		m, err := cn.state.GetMonitor(c.ctx, cluster.DefaultTenantID, consensusMonitorID)
		if err != nil {
			c.t.Fatal(err)
		}
		cn.agg.Load().Sync([]store.Monitor{m})
	}
}

// startAggregator runs the aggregator and the heartbeat feed of cn as the binary does.
func (c *consensusCluster) startAggregator(cn *consensusNode) {
	ctx, cancel := context.WithCancel(c.ctx)
	cn.halt = cancel
	agg := aggregator.New(aggregator.Options{
		State: cn.state, Metrics: cn.metrics, Cluster: countingApplier{cn: cn}, NodeID: cn.id,
		IsLeader:    cn.host.Node.IsLeader,
		Registry:    func(ctx context.Context) ([]store.Node, error) { return cn.state.ListNodes(ctx) },
		Assigner:    scheduler.Assigner{Capability: scheduler.Capabilities(checker.Default)},
		LeaderGrace: consensusGrace, Unit: consensusUnit,
		Logger: slog.New(slog.DiscardHandler),
	})
	cn.agg.Store(agg)
	cn.running.Add(2)
	go func() { defer cn.running.Done(); agg.Run(ctx) }()
	go func() { defer cn.running.Done(); c.feed(ctx, cn) }()
}

// feed stores one heartbeat per slot of the monitor grid, 10 ms into the slot,
// with the status that the test set for the voter. It hands each batch to the
// same hooks as the HeartbeatWriter: replication push and the local aggregator.
func (c *consensusCluster) feed(ctx context.Context, cn *consensusNode) {
	phase := scheduler.Phase(consensusMonitorID, consensusUnit).Milliseconds()
	unit := consensusUnit.Milliseconds()
	slot := (time.Now().UnixMilli()-phase)/unit + 1
	var seq uint64
	for {
		at := phase + slot*unit + 10
		if !sleepUntil(ctx, time.UnixMilli(at)) {
			return
		}
		seq++
		batch := []store.Heartbeat{{
			NodeID: cn.id, Incarnation: cn.metrics.Incarnation(), Seq: seq,
			TenantID: cluster.DefaultTenantID, MonitorID: consensusMonitorID, TsMs: at,
			Status: store.CheckStatus(cn.status.Load()), Details: json.RawMessage("{}"),
		}}
		if _, err := cn.metrics.InsertHeartbeats(ctx, batch); err != nil {
			if ctx.Err() == nil {
				c.t.Errorf("storing heartbeat of %s: %v", cn.id, err)
			}
			return
		}
		cn.host.Replication.Publish(batch)
		cn.observeStored(batch)
		slot++
	}
}

func sleepUntil(ctx context.Context, at time.Time) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Until(at)):
		return true
	}
}

// statusWord converts a CheckStatus to the int32 kept in the feed. The check
// makes the narrowing explicit; CheckStatus is a small enum and always fits.
func statusWord(s store.CheckStatus) int32 {
	if s < math.MinInt32 || s > math.MaxInt32 {
		panic(fmt.Sprintf("store.CheckStatus %d does not fit into int32", s))
	}
	return int32(s)
}

// report sets the status every voter reports from its next slot on.
func (c *consensusCluster) report(status store.CheckStatus) {
	for _, cn := range c.live() {
		cn.status.Store(statusWord(status))
	}
}

// live returns the voters that have not been stopped.
func (c *consensusCluster) live() []*consensusNode {
	var out []*consensusNode
	for _, cn := range c.nodes {
		if !cn.stopped {
			out = append(out, cn)
		}
	}
	return out
}

// leader returns the live voter that is leader, once one exists.
func (c *consensusCluster) leader() *consensusNode {
	c.t.Helper()
	var out *consensusNode
	eventually(c.t, "a leader among the live voters", 15*time.Second, func() bool {
		for _, cn := range c.live() {
			if cn.host.Node.IsLeader() {
				out = cn
				return true
			}
		}
		return false
	})
	return out
}

// waitStatus waits until every live voter shows the monitor with status want.
func (c *consensusCluster) waitStatus(want store.MonitorStatus, timeout time.Duration) {
	c.t.Helper()
	for _, cn := range c.live() {
		eventually(c.t, fmt.Sprintf("status %s on %s", want, cn.id), timeout, func() bool {
			m, err := cn.state.GetMonitor(c.ctx, cluster.DefaultTenantID, consensusMonitorID)
			return err == nil && m.State.Status == want
		})
	}
}

// incidents returns the incidents of the monitor as stored by the first live voter.
func (c *consensusCluster) incidents() []store.Incident {
	c.t.Helper()
	page, err := c.live()[0].state.ListIncidents(c.ctx, cluster.DefaultTenantID, store.IncidentQuery{MonitorID: consensusMonitorID})
	if err != nil {
		c.t.Fatal(err)
	}
	return page.Items
}

// stop halts the feed and the aggregator of cn, then stops the voter.
func (c *consensusCluster) stop(cn *consensusNode) {
	c.t.Helper()
	cn.halt()
	cn.running.Wait()
	cn.stopped = true
	stop(c.t, cn.testNode)
}

// shutdown halts all feeds and aggregators before the voters are closed.
func (c *consensusCluster) shutdown() {
	for _, cn := range c.nodes {
		if cn.halt != nil {
			cn.halt()
			cn.running.Wait()
		}
	}
}

// TestConsensusAcrossVoters runs three voters through three scenarios on one
// cluster: partial failure, total failure, and a leader change while DOWN.
func TestConsensusAcrossVoters(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("starts three voters")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := startConsensusCluster(ctx, t)
	defer c.shutdown()
	c.setup(store.CheckDown, store.CheckUp, store.CheckUp)

	t.Run("one DOWN of three is PARTIAL without incident", func(t *testing.T) {
		c.waitStatus(store.StatusPartial, 15*time.Second)
		time.Sleep(consensusSettle)
		c.waitStatus(store.StatusPartial, 0)
		for _, cn := range c.live() {
			if n := cn.downEvents.Load(); n != 0 {
				t.Errorf("%s: DOWN changes = %d, want 0", cn.id, n)
			}
		}
		if inc := c.incidents(); len(inc) != 0 {
			t.Errorf("incidents = %d, want 0", len(inc))
		}
	})

	t.Run("all DOWN is one DOWN change with one incident", func(t *testing.T) {
		c.report(store.CheckDown)
		for _, cn := range c.live() {
			cn.resetCounters()
		}
		c.waitStatus(store.StatusDown, 15*time.Second)
		time.Sleep(consensusSettle)
		c.waitStatus(store.StatusDown, 0)
		for _, cn := range c.live() {
			if n := cn.downEvents.Load(); n != 1 {
				t.Errorf("%s: DOWN changes = %d, want 1", cn.id, n)
			}
			if n := cn.followerProposals.Load(); n != 0 {
				t.Errorf("%s: %d status changes proposed while not leader, want 0", cn.id, n)
			}
		}
		inc := c.incidents()
		if len(inc) != 1 {
			t.Fatalf("incidents = %d, want 1", len(inc))
		}
		var want []string
		for _, cn := range c.live() {
			want = append(want, cn.id)
		}
		got := slices.Clone(inc[0].AffectedNodes)
		sort.Strings(got)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Errorf("affected nodes = %v, want %v", got, want)
		}
	})

	t.Run("leader stops while DOWN, the others report UP: one UP change", func(t *testing.T) {
		old := c.leader()
		c.report(store.CheckUp)
		for _, cn := range c.live() {
			cn.resetCounters()
		}
		c.stop(old)
		if len(c.live()) != 2 {
			t.Fatalf("live voters = %d, want 2", len(c.live()))
		}
		c.waitStatus(store.StatusUp, 20*time.Second)
		time.Sleep(consensusSettle)
		c.waitStatus(store.StatusUp, 0)
		for _, cn := range c.live() {
			if n := cn.upEvents.Load(); n != 1 {
				t.Errorf("%s: UP changes = %d, want 1", cn.id, n)
			}
			if n := cn.followerProposals.Load(); n != 0 {
				t.Errorf("%s: %d status changes proposed while not leader, want 0", cn.id, n)
			}
		}
		if inc := c.incidents(); len(inc) != 1 || inc[0].ResolvedMs == 0 {
			t.Errorf("incidents = %d (resolved %v), want one resolved incident", len(inc), len(inc) == 1 && inc[0].ResolvedMs != 0)
		}
	})
}
