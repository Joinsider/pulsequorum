// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package clusterhost

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/alert"
	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/pki"
	"github.com/joinsider/uptimecluster/internal/store"
)

// FA-ALR-03: a leader change during delivery leads to exactly one notification
// per transition and channel. A second notification is only possible when the
// sender confirmed a delivery that the cluster did not record (at least once).
//
// The tests start three voters, run the alert dispatcher on every voter as
// cmd/uptimecluster does, and count the successful sends per channel URL and
// alert ID. The dispatcher runs only where IsLeader is true.

const (
	alertMonitorID = "web"
	alertChannelA  = "chA"
	alertChannelB  = "chB"
	alertURLA      = "https://a.example.com/hook"
	alertURLB      = "https://b.example.com/hook"
)

// sendHook runs before a send and may block or fail it. A nil hook sends.
type sendHook func(ctx context.Context, t alert.Target, p cluster.AlertPayload) error

// sendLog counts the successful sends, shared by all voters.
type sendLog struct {
	mu sync.Mutex
	ok map[string]int // url + alert ID -> successful sends
}

func (l *sendLog) add(url, alertID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ok[url+"|"+alertID]++
}

func (l *sendLog) count(url, alertID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ok[url+"|"+alertID]
}

// voterSender is the Deliverer of one voter.
type voterSender struct {
	log  *sendLog
	hook sendHook
}

func (s *voterSender) Send(ctx context.Context, t alert.Target, p cluster.AlertPayload) error {
	if s.hook != nil {
		if err := s.hook(ctx, t, p); err != nil {
			return err
		}
	}
	s.log.add(t.URL, p.AlertID)
	return nil
}

// alertCluster is three voters with one dispatcher each.
type alertCluster struct {
	t       *testing.T
	ctx     context.Context
	nodes   []*testNode
	stopped map[*testNode]bool
	cancel  map[*testNode]context.CancelFunc
	log     *sendLog
}

// newAlertCluster starts three voters: n1 bootstraps, n2 joins through n1 and
// n3 through n2 (FA-CLU-03), as in TestThreeVotersJoinOverMTLS.
func newAlertCluster(ctx context.Context, t *testing.T) *alertCluster {
	t.Helper()
	n1, err := startNode(ctx, t, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n1 leader", 10*time.Second, n1.host.Node.IsLeader)
	token := func() string {
		tok, _, err := n1.host.Authority.CreateJoinToken(ctx, pki.RoleVoter, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	n2, err := startNode(ctx, t, []string{n1.addr}, token())
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n2 voter", 30*time.Second, func() bool { return isVoter(n1, n2.id) })
	n3, err := startNode(ctx, t, []string{n2.addr}, token())
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n3 voter", 30*time.Second, func() bool { return isVoter(n1, n3.id) })

	c := &alertCluster{t: t, ctx: ctx, nodes: []*testNode{n1, n2, n3},
		stopped: map[*testNode]bool{}, cancel: map[*testNode]context.CancelFunc{}, log: &sendLog{ok: map[string]int{}}}
	t.Cleanup(func() {
		for _, cancel := range c.cancel {
			cancel()
		}
	})
	return c
}

// live returns the voters that have not been stopped.
func (c *alertCluster) live() []*testNode {
	var out []*testNode
	for _, n := range c.nodes {
		if !c.stopped[n] {
			out = append(out, n)
		}
	}
	return out
}

// waitLeader returns the live voter that is leader, once one exists.
func (c *alertCluster) waitLeader() *testNode {
	c.t.Helper()
	var leader *testNode
	eventually(c.t, "a leader among the live voters", 15*time.Second, func() bool {
		for _, n := range c.live() {
			if n.host.Node.IsLeader() {
				leader = n
				return true
			}
		}
		return false
	})
	return leader
}

// setup creates the two channels and the monitor, then starts a dispatcher on
// every voter. leaderHook is used by the dispatcher of the current leader only.
func (c *alertCluster) setup(leaderHook sendHook) *testNode {
	c.t.Helper()
	leader := c.waitLeader()
	for _, ch := range []struct{ id, url string }{{alertChannelA, alertURLA}, {alertChannelB, alertURLB}} {
		enc, err := alert.EncryptURL(leader.keyring, ch.id, ch.url)
		if err != nil {
			c.t.Fatal(err)
		}
		cmd, err := cluster.NewPutChannel("", cluster.DefaultTenantID, ch.id, cluster.ChannelDocument{
			Name: ch.id, Kind: store.ChannelWebhook, URLEnc: enc, Events: []string{store.EventDown, store.EventUp},
		}, 0)
		if err != nil {
			c.t.Fatal(err)
		}
		c.apply(cmd)
	}
	doc := cluster.MonitorDocument{Name: "web", Type: "http", IntervalSeconds: 60, TimeoutSeconds: 10,
		RecoveryThreshold: 1, Assignment: store.Assignment{Mode: "all"}, ConsensusPolicy: "majority",
		NotificationChannelIDs: []string{alertChannelA, alertChannelB}}
	cmd, err := cluster.NewPutMonitor("", cluster.DefaultTenantID, alertMonitorID, doc, 0)
	if err != nil {
		c.t.Fatal(err)
	}
	c.apply(cmd)

	for _, n := range c.nodes {
		var hook sendHook
		if n == leader {
			hook = leaderHook
		}
		c.dispatch(n, hook)
	}
	return leader
}

// dispatch runs the dispatcher of n until it is stopped.
func (c *alertCluster) dispatch(n *testNode, hook sendHook) {
	ctx, cancel := context.WithCancel(c.ctx)
	c.cancel[n] = cancel
	d := alert.NewDispatcher(alert.DispatcherOptions{
		State: n.state, Cluster: n.host.Node, Keyring: n.keyring,
		Sender:   &voterSender{log: c.log, hook: hook},
		IsLeader: n.host.Node.IsLeader, Logger: slog.New(slog.DiscardHandler),
		Interval: 20 * time.Millisecond, ReminderInterval: time.Hour,
	})
	go d.Run(ctx)
}

// stopNode stops the dispatcher and then the voter. Host.Close hands over
// leadership if n is the leader (FA-NODE-04).
func (c *alertCluster) stopNode(n *testNode) {
	c.t.Helper()
	c.cancel[n]()
	c.stopped[n] = true
	stop(c.t, n)
}

// failover stops the leader and waits for another voter to become leader.
func (c *alertCluster) failover(leader *testNode) *testNode {
	c.t.Helper()
	c.stopNode(leader)
	next := c.waitLeader()
	if next == leader {
		c.t.Fatal("the stopped voter is still the leader")
	}
	return next
}

// apply proposes cmd through a live voter.
func (c *alertCluster) apply(cmd *pb.Command) {
	c.t.Helper()
	if _, err := c.live()[0].host.Node.Apply(c.ctx, cmd); err != nil {
		c.t.Fatal(err)
	}
}

// setStatus changes the status of the monitor through the FSM.
func (c *alertCluster) setStatus(from, to store.MonitorStatus, sinceMs int64) {
	c.t.Helper()
	change := cluster.StateChange{TenantID: cluster.DefaultTenantID, MonitorID: alertMonitorID, From: from, To: to, SinceMs: sinceMs}
	if to == store.StatusDown {
		change.Incident = &store.Incident{ID: uuid.NewString(), Cause: "connection refused"}
	}
	c.apply(cluster.NewSetMonitorState(change))
}

// down sets the monitor DOWN and returns the start of the DOWN period.
func (c *alertCluster) down() int64 {
	since := time.Now().UnixMilli()
	c.setStatus(store.StatusPending, store.StatusDown, since)
	return since
}

// up recovers the monitor from DOWN and returns the start of the UP period.
func (c *alertCluster) up(downSince int64) int64 {
	since := max(time.Now().UnixMilli(), downSince+1000)
	c.setStatus(store.StatusDown, store.StatusUp, since)
	return since
}

// waitDelivered waits until every live voter records the alert as delivered
// on both channels.
func (c *alertCluster) waitDelivered(key string) {
	c.t.Helper()
	for _, n := range c.live() {
		for _, ch := range []string{alertChannelA, alertChannelB} {
			eventually(c.t, fmt.Sprintf("%s delivered on %s", key, ch), 15*time.Second, func() bool {
				a, err := n.state.GetAlert(c.ctx, key, ch)
				return err == nil && a.State == store.AlertDelivered
			})
		}
	}
}

// wantSends checks the number of successful sends of the alert per channel.
func (c *alertCluster) wantSends(key string, lo, hi int) {
	c.t.Helper()
	for _, url := range []string{alertURLA, alertURLB} {
		if got := c.log.count(url, key); got < lo || got > hi {
			c.t.Errorf("sends of %s to %s = %d, want %d..%d", key, url, got, lo, hi)
		}
	}
}

func downKey(sinceMs int64) string { return fmt.Sprintf("%s:%d:DOWN", alertMonitorID, sinceMs) }
func upKey(sinceMs int64) string   { return fmt.Sprintf("%s:%d:UP", alertMonitorID, sinceMs) }

// TestLeaderChangeAlerting covers FA-ALR-03: a leader change during delivery
// produces one notification per transition and channel, and duplicates only
// when a delivery was not acknowledged in the cluster.
func TestLeaderChangeAlerting(t *testing.T) {
	if testing.Short() {
		t.Skip("starts three voters")
	}
	tests := []struct {
		name string
		run  func(t *testing.T, c *alertCluster)
	}{
		{"acked entry is not redelivered after leader stops", func(_ *testing.T, c *alertCluster) {
			c.setup(nil)
			leader := c.waitLeader()
			key := downKey(c.down())
			c.waitDelivered(key)
			c.wantSends(key, 1, 1)

			c.failover(leader)
			time.Sleep(500 * time.Millisecond) // several ticks of the new leader
			c.wantSends(key, 1, 1)
		}},
		{"blocked send, leader stops: new leader delivers once", func(t *testing.T, c *alertCluster) {
			entered := make(chan struct{}, 8)
			block := func(ctx context.Context, _ alert.Target, _ cluster.AlertPayload) error {
				entered <- struct{}{}
				<-ctx.Done()
				return ctx.Err() // never succeeded, so the old leader does not count
			}
			leader := c.setup(block)
			key := downKey(c.down())
			for range 2 {
				select {
				case <-entered:
				case <-time.After(15 * time.Second):
					t.Fatal("the leader did not start the sends")
				}
			}

			c.failover(leader)
			c.waitDelivered(key)
			c.wantSends(key, 1, 1)
		}},
		{"send confirmed, ack lost: at most one duplicate", func(t *testing.T, c *alertCluster) {
			sent := make(chan struct{}, 8)
			release := make(chan struct{})
			var once sync.Once
			releaseSends := func() { once.Do(func() { close(release) }) }
			t.Cleanup(releaseSends)
			hold := func(context.Context, alert.Target, cluster.AlertPayload) error {
				sent <- struct{}{}
				<-release
				return nil // the send succeeds, but the leader stops before it can ack
			}
			leader := c.setup(hold)
			key := downKey(c.down())
			for range 2 {
				select {
				case <-sent:
				case <-time.After(15 * time.Second):
					t.Fatal("the leader did not start the sends")
				}
			}

			c.stopNode(leader)
			releaseSends()
			if c.waitLeader() == leader {
				t.Fatal("the stopped voter is still the leader")
			}
			c.waitDelivered(key)
			c.wantSends(key, 1, 2)
		}},
		{"recovery after leader change: one UP per channel", func(_ *testing.T, c *alertCluster) {
			c.setup(nil)
			leader := c.waitLeader()
			since := c.down()
			c.waitDelivered(downKey(since))

			c.failover(leader)
			upSince := c.up(since)
			c.waitDelivered(upKey(upSince))
			c.wantSends(upKey(upSince), 1, 1)
			c.wantSends(downKey(since), 1, 1)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			c := newAlertCluster(ctx, t)
			tc.run(t, c)
		})
	}
}
