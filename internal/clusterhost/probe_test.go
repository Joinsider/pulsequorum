// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package clusterhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/checker/httpcheck"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/clusternet"
	"github.com/joinsider/pulsequorum/internal/clusterpki"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/probe"
	"github.com/joinsider/pulsequorum/internal/scheduler"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

type testProbe struct {
	id          string
	incarnation string
	dir         string
	metrics     *sqlite.MetricsStore
	cert        *clusterpki.NodeCert
	voters      *probe.VoterList
	buf         *probe.Buffer
	sched       *scheduler.Scheduler
	schedCancel context.CancelFunc
	schedWg     sync.WaitGroup
	client      *probe.Client
	cancel      context.CancelFunc
	errCh       chan error
	wg          sync.WaitGroup
}

type testProbeOptions struct {
	name           string
	region         string
	tenantID       string
	localSSRF      bool
	unit           time.Duration
	ackRetain      time.Duration
	maxAge         time.Duration
	maxBytes       int64
	sessionTimeout time.Duration
	outage         *outageController
}

type outageController struct {
	blocked atomic.Bool
	mu      sync.Mutex
	conns   []*grpc.ClientConn
}

func (o *outageController) track(conn *grpc.ClientConn) {
	o.mu.Lock()
	o.conns = append(o.conns, conn)
	o.mu.Unlock()
}

func (o *outageController) setOutage(down bool) {
	o.blocked.Store(down)
	if !down {
		return
	}
	o.mu.Lock()
	conns := o.conns
	o.conns = nil
	o.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func startThreeVotersForProbes(ctx context.Context, t *testing.T, replInterval time.Duration) []*testNode {
	t.Helper()

	n1, err := startNodeInterval(ctx, t, nil, "", replInterval)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n1 leader", 15*time.Second, n1.host.Node.IsLeader)

	token := func() string {
		tok, _, err := n1.host.Authority.CreateJoinToken(ctx, pki.RoleVoter, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	n2, err := startNodeInterval(ctx, t, []string{n1.addr}, token(), replInterval)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n2 promoted to voter", 30*time.Second, func() bool { return isVoter(n1, n2.id) })

	n3, err := startNodeInterval(ctx, t, []string{n2.addr}, token(), replInterval)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n3 promoted to voter", 30*time.Second, func() bool { return isVoter(n1, n3.id) })

	voters := []*testNode{n1, n2, n3}
	eventually(t, "command level reaches probe level", 30*time.Second, func() bool {
		for _, v := range voters {
			lvl, err := v.host.Node.CommandLevel(ctx)
			if err != nil || lvl < cluster.ProbeCommandLevel {
				return false
			}
		}
		return true
	})
	return voters
}

func startTestProbe(ctx context.Context, t *testing.T, leader *testNode, opts testProbeOptions) *testProbe {
	t.Helper()

	var actor *pb.Actor
	if opts.tenantID != "" {
		actor = &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-admin", Name: "Admin"}
	}
	tok, _, err := leader.host.Authority.CreateJoinTokenFor(ctx, pki.RoleProbe, opts.tenantID, 0, actor)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	nodeID := uuid.NewString()
	log := slog.New(slog.DiscardHandler)

	metrics, err := sqlite.OpenMetrics(ctx, dir, sqlite.MetricsOptions{
		ReadConns:    1,
		CacheSizeKiB: sqlite.ProbeMetricsCacheKiB,
	})
	if err != nil {
		t.Fatal(err)
	}

	caps := []string{httpcheck.ClientCertCapability}
	cert, voters, err := probe.Prepare(ctx, probe.PrepareOptions{
		DataDir:      dir,
		NodeID:       nodeID,
		Name:         opts.name,
		Seeds:        []string{leader.addr},
		Token:        tok,
		Region:       opts.region,
		Capabilities: caps,
		Version:      "test",
		Incarnation:  metrics.Incarnation(),
		Log:          log,
	})
	if err != nil {
		_ = metrics.Close()
		t.Fatal(err)
	}

	policy := probe.NewPolicy(opts.localSSRF, opts.tenantID != "")
	checkEnv := &checker.Env{
		Dialer:    &checker.Dialer{Policy: policy},
		UserAgent: "Pulsequorum-Test/1.0",
		Now:       time.Now,
	}

	hw, err := scheduler.NewHeartbeatWriter(ctx, metrics, nodeID, log)
	if err != nil {
		_ = metrics.Close()
		t.Fatal(err)
	}

	ackRetain := opts.ackRetain
	if ackRetain <= 0 {
		ackRetain = 5 * time.Second
	}
	buf, err := probe.NewBuffer(ctx, probe.BufferOptions{
		Store:     metrics,
		NodeID:    nodeID,
		AckRetain: ackRetain,
		MaxAge:    opts.maxAge,
		MaxBytes:  opts.maxBytes,
		Log:       log,
	})
	if err != nil {
		_ = metrics.Close()
		t.Fatal(err)
	}

	sched := scheduler.New(scheduler.Options{
		Registry: checker.Default,
		Env:      checkEnv,
		Sink:     hw,
		Logger:   log,
		Unit:     opts.unit,
	})

	var dialFn func(context.Context, string) (*grpc.ClientConn, error)
	if opts.outage != nil {
		verifier := &pki.Verifier{
			ClusterIDFunc: func() string {
				if leaf := cert.Leaf(); leaf != nil {
					if id, err := pki.IdentityOf(leaf); err == nil {
						return id.ClusterID
					}
				}
				return ""
			},
			Roots: cert.Bundle,
		}
		sec := clusternet.Security{Holder: &cert.Holder, Verifier: verifier}
		dialFn = func(_ context.Context, addr string) (*grpc.ClientConn, error) {
			if opts.outage.blocked.Load() {
				return nil, errors.New("simulated WAN outage")
			}
			conn, err := clusternet.DialProbe(addr, sec)
			if err != nil {
				return nil, err
			}
			opts.outage.track(conn)
			return conn, nil
		}
	}

	client, err := probe.NewClient(probe.ClientOptions{
		NodeID:         nodeID,
		Name:           opts.name,
		Region:         opts.region,
		Capabilities:   caps,
		Version:        "test",
		Cert:           cert,
		Voters:         voters,
		Buffer:         buf,
		Metrics:        metrics,
		Sync:           sched,
		Policy:         policy,
		SessionTimeout: opts.sessionTimeout,
		ConnectTimeout: 3 * time.Second,
		BackoffMin:     25 * time.Millisecond,
		BackoffMax:     100 * time.Millisecond,
		Dial:           dialFn,
		Log:            log,
	})
	if err != nil {
		_ = metrics.Close()
		t.Fatal(err)
	}
	hw.SetOnStored(client.NotifyNewHeartbeats)

	pCtx, cancel := context.WithCancel(ctx)
	schedCtx, schedCancel := context.WithCancel(pCtx)
	p := &testProbe{
		id:          nodeID,
		incarnation: metrics.Incarnation(),
		dir:         dir,
		metrics:     metrics,
		cert:        cert,
		voters:      voters,
		buf:         buf,
		sched:       sched,
		schedCancel: schedCancel,
		client:      client,
		cancel:      cancel,
		errCh:       make(chan error, 1),
	}

	p.wg.Go(func() { hw.Run(pCtx) })
	p.schedWg.Go(func() { sched.Run(schedCtx) })
	p.wg.Go(func() { p.errCh <- client.Run(pCtx) })

	t.Cleanup(func() {
		p.stop()
		_ = metrics.Close()
	})

	return p
}

func (p *testProbe) stopChecks() {
	p.schedCancel()
	p.schedWg.Wait()
}

func (p *testProbe) stop() {
	p.stopChecks()
	p.cancel()
	p.wg.Wait()
}

func findLeader(nodes []*testNode) *testNode {
	for _, n := range nodes {
		if n.host.Node.IsLeader() {
			return n
		}
	}
	return nil
}

func putHTTPMonitor(ctx context.Context, t *testing.T, leader *testNode, id, rawURL string, rev uint64) uint64 {
	t.Helper()
	cfgJSON, err := json.Marshal(map[string]any{"url": rawURL})
	if err != nil {
		t.Fatal(err)
	}
	doc := cluster.MonitorDocument{
		Name:              id,
		Type:              "http",
		Config:            cfgJSON,
		IntervalSeconds:   1,
		TimeoutSeconds:    2,
		Retries:           1,
		RecoveryThreshold: 1,
		Assignment:        store.Assignment{Mode: "all"},
		ConsensusPolicy:   "majority",
	}
	cmd, err := cluster.NewPutMonitor("put-"+id+"-"+uuid.NewString(), cluster.DefaultTenantID, id, doc, rev)
	if err != nil {
		t.Fatal(err)
	}
	res, err := leader.host.Node.Apply(ctx, cmd)
	if err != nil {
		t.Fatalf("apply monitor %s: %v", id, err)
	}
	if res.Err != nil {
		t.Fatalf("apply monitor %s command error: %v", id, res.Err)
	}
	m, err := leader.state.GetMonitor(ctx, cluster.DefaultTenantID, id)
	if err != nil {
		t.Fatalf("read monitor %s after apply: %v", id, err)
	}
	return m.Revision
}

func readAllHeartbeats(ctx context.Context, m *sqlite.MetricsStore, nodeID, inc string) ([]store.Heartbeat, error) {
	var all []store.Heartbeat
	var after uint64
	for {
		batch, more, err := m.HeartbeatsAfter(ctx, nodeID, inc, after, math.MaxInt64, 500)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if !more {
			return all, nil
		}
		after = batch[len(batch)-1].Seq
	}
}

func assertContiguousPrefix(t *testing.T, where string, hbs []store.Heartbeat, wantMin uint64) {
	t.Helper()
	if uint64(len(hbs)) < wantMin {
		t.Fatalf("%s: got %d heartbeats, want >= %d", where, len(hbs), wantMin)
	}
	for i, h := range hbs {
		wantSeq := uint64(i + 1)
		if h.Seq != wantSeq {
			t.Fatalf("%s: heartbeat[%d].Seq = %d, want %d (gap or duplicate)", where, i, h.Seq, wantSeq)
		}
	}
}

// TestProbeEndToEndAndFailover covers ADR 0034 end-to-end across 3 voters and
// 2 probes over mTLS: join, snapshot & delta push, heartbeat replication,
// failover on connected voter & leader loss, and clean exit on revocation.
func TestProbeEndToEndAndFailover(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	voters := startThreeVotersForProbes(ctx, t, 200*time.Millisecond)
	leader := voters[0]

	var hits1 atomic.Int64
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits1.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv1.Close()

	var hits2 atomic.Int64
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits2.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv2.Close()

	p1 := startTestProbe(ctx, t, leader, testProbeOptions{
		name:   "probe-1",
		region: "eu-central",
		unit:   100 * time.Millisecond,
	})
	p2 := startTestProbe(ctx, t, voters[1], testProbeOptions{
		name:   "probe-2",
		region: "us-east",
		unit:   100 * time.Millisecond,
	})

	// Both probes register with healthy=true and learn all voter addresses.
	eventually(t, "both probes registered healthy", 15*time.Second, func() bool {
		reg := registry(t, leader)
		n1, ok1 := reg[p1.id]
		n2, ok2 := reg[p2.id]
		return ok1 && ok2 && n1.Role == store.NodeRoleProbe && n2.Role == store.NodeRoleProbe && n1.Healthy && n2.Healthy
	})

	rev := putHTTPMonitor(ctx, t, leader, "mon-e2e", srv1.URL, 0)
	eventually(t, "probes execute srv1", 10*time.Second, func() bool {
		return hits1.Load() >= 4
	})

	// Config delta reaches probes within <= 5s.
	deltaStart := time.Now()
	_ = putHTTPMonitor(ctx, t, leader, "mon-e2e", srv2.URL, rev)
	eventually(t, "probes pick up config delta <= 5s", 5*time.Second, func() bool {
		return hits2.Load() >= 2
	})
	t.Logf("config delta applied in %s", time.Since(deltaStart))

	// Both probe streams replicate to all 3 voters without gaps.
	for _, p := range []*testProbe{p1, p2} {
		eventually(t, fmt.Sprintf("probe %s heartbeats replicated to all voters", p.id), 15*time.Second, func() bool {
			for _, v := range voters {
				last, err := v.metrics.LastSeq(ctx, p.id, p.incarnation)
				if err != nil || last < 3 {
					return false
				}
			}
			return true
		})
	}

	// Kill the voter currently serving p1 and force leader failover as well.
	var connectedVoter *testNode
	eventually(t, "p1 session visible", 10*time.Second, func() bool {
		sess, ok := leader.host.ProbeSessions()[p1.id]
		if !ok || sess.ConnectedVoterID == "" {
			return false
		}
		for _, v := range voters {
			if v.id == sess.ConnectedVoterID {
				connectedVoter = v
				return true
			}
		}
		return false
	})

	beforeFailover := hits2.Load()
	wasLeader := connectedVoter.id == leader.id
	stop(t, connectedVoter)

	var survivors []*testNode
	for _, v := range voters {
		if v.id != connectedVoter.id {
			survivors = append(survivors, v)
		}
	}

	// If p1's connected voter was a follower, bring it back first and then stop
	// the leader too so both connected-voter failover and leader loss are exercised.
	if !wasLeader {
		eventually(t, "p1 fails over within <30s after connected voter stop", 25*time.Second, func() bool {
			return hits2.Load() > beforeFailover+2 && p1.client.Ready() == nil
		})
		connectedVoter = restart(ctx, t, connectedVoter)
		eventually(t, "restarted voter rejoins and catches up", 20*time.Second, func() bool {
			if !isVoter(survivors[0], connectedVoter.id) {
				return false
			}
			_, err := connectedVoter.state.GetMonitor(ctx, cluster.DefaultTenantID, "mon-e2e")
			return err == nil
		})
		voters = make([]*testNode, 0, 3)
		voters = append(voters, survivors...)
		voters = append(voters, connectedVoter)
		eventually(t, "leader active across restarted cluster", 10*time.Second, func() bool {
			return findLeader(voters) != nil
		})

		// Now kill the leader to exercise leader failover while maintaining 2/3 quorum.
		curLeader := findLeader(voters)
		if curLeader == nil {
			t.Fatal("expected active leader before leader kill")
		}
		beforeLeaderKill := hits2.Load()
		stop(t, curLeader)
		survivors = nil
		for _, v := range voters {
			if v.id != curLeader.id {
				survivors = append(survivors, v)
			}
		}
		eventually(t, "surviving voters elect new leader", 15*time.Second, func() bool {
			return findLeader(survivors) != nil
		})
		eventually(t, "probes resume uploading after leader failover", 25*time.Second, func() bool {
			return hits2.Load() > beforeLeaderKill+2 && p1.client.Ready() == nil && p2.client.Ready() == nil
		})
	} else {
		eventually(t, "surviving voters elect new leader after connected leader stop", 15*time.Second, func() bool {
			return findLeader(survivors) != nil
		})
		eventually(t, "p1 fails over within <30s", 25*time.Second, func() bool {
			return hits2.Load() > beforeFailover+2 && p1.client.Ready() == nil
		})
	}

	// Surviving voters agree on contiguous prefix of (p1, incarnation) heartbeats.
	for _, p := range []*testProbe{p1, p2} {
		eventually(t, "survivors hold contiguous replicated heartbeats", 15*time.Second, func() bool {
			ref, err := readAllHeartbeats(ctx, survivors[0].metrics, p.id, p.incarnation)
			if err != nil || len(ref) < 5 {
				return false
			}
			other, err := readAllHeartbeats(ctx, survivors[1].metrics, p.id, p.incarnation)
			if err != nil || len(other) < len(ref) {
				return false
			}
			for i := range ref {
				if ref[i].Seq != uint64(i+1) || other[i].Seq != ref[i].Seq ||
					other[i].TsMs != ref[i].TsMs || other[i].Status != ref[i].Status {
					return false
				}
			}
			return true
		})
	}

	// Removing p1 revokes its access, closes its live stream and returns ErrCertRejected.
	activeLeader := findLeader(survivors)
	if activeLeader == nil {
		t.Fatal("expected active leader before probe removal")
	}
	beforeVoters := memberCount(activeLeader)
	removal, err := activeLeader.host.Manager.Remove(ctx, p1.id, "")
	if err != nil {
		t.Fatalf("remove probe p1: %v", err)
	}
	if removal.VotersLeft != beforeVoters {
		t.Errorf("votersLeft after probe removal = %d, want %d", removal.VotersLeft, beforeVoters)
	}
	for _, s := range survivors {
		if err := s.host.Revocations.Reload(ctx, s.state); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case err := <-p1.errCh:
		if !errors.Is(err, probe.ErrCertRejected) {
			t.Fatalf("p1 exit err = %v, want probe.ErrCertRejected", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for removed probe p1 to exit with ErrCertRejected")
	}

	eventually(t, "p1 deleted from node registry", 10*time.Second, func() bool {
		_, exists := registry(t, activeLeader)[p1.id]
		return !exists
	})
}

// TestProbeQuorumNeutrality verifies FA-PRB-07: probes never become Raft
// voters or non-voters and neither help reach nor preserve quorum.
func TestProbeQuorumNeutrality(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	voters := startThreeVotersForProbes(ctx, t, 300*time.Millisecond)
	leader := voters[0]

	const probeCount = 5
	probes := make([]*testProbe, probeCount)
	for i := range probeCount {
		probes[i] = startTestProbe(ctx, t, leader, testProbeOptions{
			name:   fmt.Sprintf("quorum-probe-%d", i+1),
			region: "eu-central",
			unit:   time.Second,
		})
	}

	// All 5 probes register and hold live sessions, while Members() still has only 3 voters.
	eventually(t, "all 5 probes active", 15*time.Second, func() bool {
		if memberCount(leader) != 3 {
			return false
		}
		for _, m := range func() []cluster.Member {
			ms, _, _ := leader.host.Node.Members()
			return ms
		}() {
			if !m.Voter {
				return false
			}
		}
		return len(leader.host.ProbeSessions()) == probeCount
	})

	// Stop all 5 probes: the 3 voters retain leader, quorum and command level.
	for _, p := range probes {
		p.stop()
	}
	cmd, err := cluster.NewPutTenant("q-probe-down", "tenant-q1", cluster.TenantDocument{Name: "Q1", Plan: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leader.host.Node.Apply(ctx, cmd); err != nil {
		t.Fatalf("write failed after stopping all 5 probes: %v", err)
	}
	requireLevel(ctx, t, voters, cluster.CommandVersion, time.Second)

	// Re-connect 5 fresh probes, then stop 2 voters: 1 voter + 5 probes must lose quorum.
	for i := range probeCount {
		probes[i] = startTestProbe(ctx, t, leader, testProbeOptions{
			name:   fmt.Sprintf("quorum-probe-2-%d", i+1),
			region: "eu-central",
			unit:   time.Second,
		})
	}
	eventually(t, "5 probes active before voter outage", 15*time.Second, func() bool {
		return len(leader.host.ProbeSessions()) == probeCount
	})

	stop(t, voters[1])
	stop(t, voters[2])

	eventually(t, "remaining single voter loses quorum despite 5 probes", 10*time.Second, func() bool {
		writeCtx, wCancel := context.WithTimeout(ctx, time.Second)
		defer wCancel()
		c, _ := cluster.NewPutTenant("q-no-quorum", "tenant-nq", cluster.TenantDocument{Name: "NQ", Plan: "default"})
		_, applyErr := leader.host.Node.Apply(writeCtx, c)
		return applyErr != nil
	})
}

// TestProbeAS08 covers the shortened AS-08 acceptance test (FA-PRB-02/03):
// prolonged WAN outage buffers hundreds of intervals in local SQLite, drains
// completely in order upon reconnect with zero gaps/duplicates/drops, and the
// overflow subtest exercises MaxAge expiry + lost_below progress.
func TestProbeAS08(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	voters := startThreeVotersForProbes(ctx, t, 150*time.Millisecond)
	leader := voters[0]

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_ = putHTTPMonitor(ctx, t, leader, "mon-as08", srv.URL, 0)

	t.Run("outage drain without loss", func(t *testing.T) {
		outage := &outageController{}
		p := startTestProbe(ctx, t, leader, testProbeOptions{
			name:      "as08-drain",
			region:    "eu-central",
			unit:      10 * time.Millisecond,
			ackRetain: time.Second,
			maxAge:    time.Hour,
			maxBytes:  50 << 20,
			outage:    outage,
		})

		eventually(t, "initial heartbeats uploaded before outage", 15*time.Second, func() bool {
			return p.buf.AckedSeq() >= 5
		})

		// Simulate WAN outage long enough to buffer 360+ check intervals locally.
		outage.setOutage(true)
		startOutageSeq := p.buf.AckedSeq()
		eventually(t, "360+ intervals buffered during outage", 15*time.Second, func() bool {
			last, err := p.metrics.LastSeq(ctx, p.id, p.incarnation)
			return err == nil && last >= startOutageSeq+360
		})
		bufferedTarget, err := p.metrics.LastSeq(ctx, p.id, p.incarnation)
		if err != nil {
			t.Fatal(err)
		}

		// Restore connectivity and let the probe drain and replicate the backlog.
		outage.setOutage(false)
		eventually(t, "probe drains backlog to all 3 voters", 30*time.Second, func() bool {
			for _, v := range voters {
				last, err := v.metrics.LastSeq(ctx, p.id, p.incarnation)
				if err != nil || last < bufferedTarget {
					return false
				}
			}
			return true
		})

		if dropped := p.buf.DroppedTotal(); dropped != 0 {
			t.Errorf("dropped_total = %d, want 0", dropped)
		}

		// Quiesce checks while HeartbeatWriter flushes and Client uploads everything.
		p.stopChecks()
		time.Sleep(1200 * time.Millisecond)
		probeLast, err := p.metrics.LastSeq(ctx, p.id, p.incarnation)
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "all final probe heartbeats replicated to all 3 voters", 20*time.Second, func() bool {
			if p.buf.AckedSeq() < probeLast {
				return false
			}
			for _, v := range voters {
				last, err := v.metrics.LastSeq(ctx, p.id, p.incarnation)
				if err != nil || last != probeLast {
					return false
				}
			}
			return true
		})
		p.stop()

		var ref []store.Heartbeat
		for i, v := range voters {
			hbs, err := readAllHeartbeats(ctx, v.metrics, p.id, p.incarnation)
			if err != nil {
				t.Fatal(err)
			}
			assertContiguousPrefix(t, v.id, hbs, probeLast)
			if uint64(len(hbs)) != probeLast {
				t.Fatalf("voter %s count = %d, max seq = %d", v.id, len(hbs), probeLast)
			}
			if i == 0 {
				ref = hbs
				continue
			}
			for j := range ref {
				if hbs[j].TsMs != ref[j].TsMs || hbs[j].Status != ref[j].Status || hbs[j].MonitorID != ref[j].MonitorID {
					t.Fatalf("voter %s mismatch at seq %d", v.id, hbs[j].Seq)
				}
			}
		}
	})

	t.Run("overflow drops expired heartbeats and advances lost_below", func(t *testing.T) {
		outage := &outageController{}
		p := startTestProbe(ctx, t, leader, testProbeOptions{
			name:      "as08-overflow",
			region:    "eu-central",
			unit:      10 * time.Millisecond,
			ackRetain: time.Nanosecond,
			maxAge:    300 * time.Millisecond,
			maxBytes:  50 << 20,
			outage:    outage,
		})

		eventually(t, "overflow probe initial upload", 15*time.Second, func() bool {
			return p.buf.AckedSeq() >= 5
		})

		outage.setOutage(true)
		eventually(t, "buffer accumulates unacked rows", 10*time.Second, func() bool {
			last, err := p.metrics.LastSeq(ctx, p.id, p.incarnation)
			return err == nil && last >= p.buf.AckedSeq()+20
		})
		// Wait past maxAge while new checks continue entering the buffer.
		time.Sleep(400 * time.Millisecond)
		if err := p.buf.Cleanup(ctx); err != nil {
			t.Fatalf("cleanup during outage: %v", err)
		}

		dropped := p.buf.DroppedTotal()
		lostBelow := p.buf.LostBelow()
		if dropped == 0 || lostBelow <= 1 {
			t.Fatalf("expected buffer overflow drop, got dropped_total=%d lost_below=%d", dropped, lostBelow)
		}

		// Reconnect: lost_below advances voter stream position so newer heartbeats upload.
		outage.setOutage(false)
		wantSeq := lostBelow + 25
		eventually(t, "subsequent heartbeats upload cleanly after drop gap", 20*time.Second, func() bool {
			if p.buf.AckedSeq() < wantSeq {
				return false
			}
			for _, v := range voters {
				last, err := v.metrics.LastSeq(ctx, p.id, p.incarnation)
				if err != nil || last < wantSeq {
					return false
				}
			}
			return true
		})
	})
}
