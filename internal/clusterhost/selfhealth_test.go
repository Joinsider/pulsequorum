// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package clusterhost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/aggregator"
	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/scheduler"
	"github.com/joinsider/pulsequorum/internal/selfhealth"
	"github.com/joinsider/pulsequorum/internal/store"
)

// ADR 0022 and AS-05: a voter whose heartbeats show DOWN while its peers show
// UP is impaired by the leader and left out of the consensus. A majority
// monitor does not go DOWN, and a monitor with policy "any" returns from DOWN
// to UP once the voter is excluded.
const (
	selfhealMajority = consensusMonitorID // "api", majority
	selfhealAny      = "policy-any"
	selfhealUnit     = consensusUnit
)

// shNode is a voter of the self-heal test: its status per monitor and its
// heartbeat sequence, which is shared by all monitors of the node.
type shNode struct {
	cn       *consensusNode
	status   map[string]*atomic.Int32
	sequence atomic.Uint64
}

// shFeed stores one heartbeat per slot of one monitor on a node.
func (sn *shNode) feed(ctx context.Context, t *testing.T, monitorID string) {
	cn := sn.cn
	phase := scheduler.Phase(monitorID, selfhealUnit).Milliseconds()
	unit := selfhealUnit.Milliseconds()
	slot := (time.Now().UnixMilli()-phase)/unit + 1
	for {
		at := phase + slot*unit + 10
		if !sleepUntil(ctx, time.UnixMilli(at)) {
			return
		}
		batch := []store.Heartbeat{{
			NodeID: cn.id, Incarnation: cn.metrics.Incarnation(), Seq: sn.sequence.Add(1),
			TenantID: cluster.DefaultTenantID, MonitorID: monitorID, TsMs: at,
			Status: store.CheckStatus(sn.status[monitorID].Load()), Details: json.RawMessage("{}"),
		}}
		if _, err := cn.metrics.InsertHeartbeats(ctx, batch); err != nil {
			if ctx.Err() == nil {
				t.Errorf("storing heartbeat of %s: %v", cn.id, err)
			}
			return
		}
		cn.host.Replication.Publish(batch)
		cn.observeStored(batch)
		slot++
	}
}

// startSelfHeal runs the aggregator, the assessor and one feed per monitor of
// a voter, as cmd/pulsequorum does. The assessor's Exclude feeds the aggregator.
func startSelfHeal(ctx context.Context, t *testing.T, sn *shNode) {
	t.Helper()
	cn := sn.cn
	ctx, cancel := context.WithCancel(ctx)
	cn.halt = cancel
	discard := slog.New(slog.DiscardHandler)
	as := selfhealth.New(selfhealth.Options{
		NodeID: cn.id, IsLeader: cn.host.Node.IsLeader, Registry: cn.state, Cluster: cn.host.Node,
		Deviation: func(sinceMs int64) map[string]aggregator.DeviationStats {
			if a := cn.agg.Load(); a != nil {
				return a.Deviation(sinceMs)
			}
			return nil
		},
		FailRatio: 0.5, Interval: 500 * time.Millisecond, Logger: discard,
	})
	agg := aggregator.New(aggregator.Options{
		State: cn.state, Metrics: cn.metrics, Cluster: countingApplier{cn: cn}, NodeID: cn.id,
		IsLeader:    cn.host.Node.IsLeader,
		Registry:    func(ctx context.Context) ([]store.Node, error) { return cn.state.ListNodes(ctx) },
		Assigner:    scheduler.Assigner{Capability: scheduler.Capabilities(checker.Default)},
		Exclude:     as.Exclude,
		LeaderGrace: consensusGrace, Unit: selfhealUnit, Logger: discard,
	})
	cn.agg.Store(agg)
	cn.running.Add(2 + len(sn.status))
	go func() { defer cn.running.Done(); agg.Run(ctx) }()
	go func() { defer cn.running.Done(); as.Run(ctx) }()
	for id := range sn.status {
		go func() { defer cn.running.Done(); sn.feed(ctx, t, id) }()
	}
}

// waitMonitor waits until the monitor shows want on every node in nodes.
func waitMonitor(ctx context.Context, t *testing.T, nodes []*shNode, monitorID string, want store.MonitorStatus, timeout time.Duration) {
	t.Helper()
	for _, sn := range nodes {
		eventually(t, fmt.Sprintf("%s %s on %s", monitorID, want, sn.cn.id), timeout, func() bool {
			m, err := sn.cn.state.GetMonitor(ctx, cluster.DefaultTenantID, monitorID)
			return err == nil && m.State.Status == want
		})
	}
}

func TestSelfHealExcludesDeviatingVoter(t *testing.T) {
	if testing.Short() {
		t.Skip("starts three voters")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := startConsensusCluster(ctx, t)
	defer c.shutdown()
	eventually(t, "command level ≥ 3 (SetNodeConnectivity)", 30*time.Second, func() bool {
		level, err := c.nodes[0].host.Node.CommandLevel(ctx)
		return err == nil && level >= 3
	})

	for _, id := range []string{selfhealMajority, selfhealAny} {
		policy := "majority"
		if id == selfhealAny {
			policy = "any"
		}
		doc := cluster.MonitorDocument{Name: id, Type: "http", IntervalSeconds: 1, TimeoutSeconds: 1,
			Retries: 0, RecoveryThreshold: 1, Assignment: store.Assignment{Mode: "all"}, ConsensusPolicy: policy}
		cmd, err := cluster.NewPutMonitor("", cluster.DefaultTenantID, id, doc, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.nodes[0].host.Node.Apply(ctx, cmd); err != nil {
			t.Fatal(err)
		}
	}
	nodes := make([]*shNode, 0, len(c.nodes))
	for _, cn := range c.nodes {
		sn := &shNode{cn: cn, status: map[string]*atomic.Int32{}}
		for _, id := range []string{selfhealMajority, selfhealAny} {
			sn.status[id] = new(atomic.Int32)
			sn.status[id].Store(int32(store.CheckUp))
		}
		nodes = append(nodes, sn)
	}
	for _, sn := range nodes {
		for _, id := range []string{selfhealMajority, selfhealAny} {
			eventually(t, "monitor "+id+" replicated to "+sn.cn.id, 15*time.Second, func() bool {
				_, err := sn.cn.state.GetMonitor(ctx, cluster.DefaultTenantID, id)
				return err == nil
			})
		}
	}
	for _, sn := range nodes {
		var monitors []store.Monitor
		for _, id := range []string{selfhealMajority, selfhealAny} {
			m, err := sn.cn.state.GetMonitor(ctx, cluster.DefaultTenantID, id)
			if err != nil {
				t.Fatal(err)
			}
			monitors = append(monitors, m)
		}
		startSelfHeal(ctx, t, sn)
		sn.cn.agg.Load().Sync(monitors)
	}

	waitMonitor(ctx, t, nodes, selfhealMajority, store.StatusUp, 20*time.Second)
	waitMonitor(ctx, t, nodes, selfhealAny, store.StatusUp, 20*time.Second)

	// Fault: one follower reports DOWN for all monitors, the others UP.
	leader := c.leader()
	var faulty *shNode
	for _, sn := range nodes {
		if sn.cn != leader {
			faulty = sn
			break
		}
	}
	for _, id := range []string{selfhealMajority, selfhealAny} {
		faulty.status[id].Store(int32(store.CheckDown))
	}
	for _, cn := range c.nodes {
		cn.resetCounters()
	}

	// Without exclusion the policy "any" monitor goes DOWN; then the leader
	// impairs the faulty voter and the monitor returns to UP.
	waitMonitor(ctx, t, nodes, selfhealAny, store.StatusDown, 20*time.Second)
	waitImpaired(ctx, t, nodes, faulty.cn.id, 60*time.Second)
	waitMonitor(ctx, t, nodes, selfhealAny, store.StatusUp, 30*time.Second)
	time.Sleep(3 * selfhealUnit)
	waitMonitor(ctx, t, nodes, selfhealAny, store.StatusUp, 0)

	// AS-05: the majority monitor never went DOWN, on any voter.
	for _, cn := range c.live() {
		if n := cn.downEvents.Load(); n != 0 {
			t.Errorf("%s: DOWN changes of the majority monitor = %d, want 0", cn.id, n)
		}
	}
	m, err := c.live()[0].state.GetMonitor(ctx, cluster.DefaultTenantID, selfhealMajority)
	if err != nil {
		t.Fatal(err)
	}
	if m.State.Status == store.StatusDown {
		t.Errorf("majority monitor is DOWN")
	}
	// Only the faulty voter is impaired, and the reason says why.
	for _, cn := range c.live() {
		reg := registry(t, cn.testNode)
		for id, n := range reg {
			if want := id == faulty.cn.id; n.ConnectivityImpaired != want {
				t.Errorf("on %s: node %s impaired = %v, want %v", cn.id, id, n.ConnectivityImpaired, want)
			}
		}
		if n := reg[faulty.cn.id]; n.ConnectivityReason != cluster.ConnectivityReasonDeviation {
			t.Errorf("on %s: reason = %q, want %q", cn.id, n.ConnectivityReason, cluster.ConnectivityReasonDeviation)
		}
	}
}

// waitImpaired waits until the registry of every live voter shows id impaired.
func waitImpaired(ctx context.Context, t *testing.T, nodes []*shNode, id string, timeout time.Duration) {
	t.Helper()
	for _, sn := range nodes {
		eventually(t, fmt.Sprintf("node %s impaired on %s", id, sn.cn.id), timeout, func() bool {
			nodes, err := sn.cn.state.ListNodes(ctx)
			if err != nil {
				return false
			}
			for _, n := range nodes {
				if n.ID == id {
					return n.ConnectivityImpaired
				}
			}
			return false
		})
	}
}
