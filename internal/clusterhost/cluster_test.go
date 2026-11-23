// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package clusterhost

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/clusternet"
	"github.com/joinsider/pulsequorum/internal/membership"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/scheduler"
	"github.com/joinsider/pulsequorum/internal/secrets"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

// Test values for the production intervals that Options makes overridable
// (zero = production default). They keep the real-Raft tests in this package
// fast without changing what they prove.
const (
	// testStopGrace replaces the 5 s graceful stop of the cluster port; the
	// streams of the stopped voter are then cut after 100 ms.
	testStopGrace = 100 * time.Millisecond
	// testHealthThreshold is the time after which a silent voter counts as
	// unhealthy (ADR 0017: 10 x the test HeartbeatTimeout of 200 ms).
	testHealthThreshold = 2 * time.Second
	// testPromotePoll is the period in which the leader promotes joined
	// non-voters; the production default is 2 s.
	testPromotePoll = 100 * time.Millisecond
	// testRevocationReload is the period in which revoked certificates reach
	// the handshakes; the production default is 10 s.
	testRevocationReload = 500 * time.Millisecond
)

type testNode struct {
	id      string
	addr    string
	dir     string
	state   *sqlite.StateStore
	metrics *sqlite.MetricsStore
	kek     *secrets.KEK
	keyring *secrets.Keyring
	host    *Host
	cancel  context.CancelFunc
	// lastIssuedAtStart is last_issued_at read before Start, as main.go does
	// for the gap job (ADR 0033).
	lastIssuedAtStart int64
	// replInterval is the anti-entropy period; zero keeps the default.
	replInterval time.Duration
	// stopGrace is Options.StopGrace of the node; zero means testStopGrace.
	stopGrace time.Duration
	// promotePoll is Options.PromotePoll of the node; zero means testPromotePoll.
	promotePoll time.Duration
	// caps are the node capabilities the node registers with (ADR 0017).
	caps []string
	// maxVersion overrides the command version of the node; zero keeps
	// cluster.CommandVersion (ADR 0018).
	maxVersion uint32
	// clock is the wall clock of the node; nil keeps the system clock (ADR 0020).
	clock func() time.Time
	// clockInterval is the period of the clock offset measurement; zero keeps
	// the default.
	clockInterval time.Duration
	// onStored receives the heartbeats that replication stored (Options.OnHeartbeatsStored).
	onStored func([]store.Heartbeat)
	// Optional probe & SSRF overrides for private probe tests.
	ssrfProtection      bool
	probeHealthInterval time.Duration
	onProbeRejected     func(string)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

// startNode assembles a voter like the binary does (cmd/pulsequorum).
func startNode(ctx context.Context, t *testing.T, seeds []string, token string) (*testNode, error) {
	t.Helper()
	return startNodeInterval(ctx, t, seeds, token, 0)
}

// startNodeWithCaps is startNode for a node with the given capabilities.
func startNodeWithCaps(ctx context.Context, t *testing.T, seeds []string, token string, caps []string) (*testNode, error) {
	t.Helper()
	return launch(ctx, t, &testNode{id: uuid.NewString(), addr: freeAddr(t), dir: t.TempDir(), caps: caps}, seeds, token)
}

// startNodeInterval is startNode with a replication interval (zero = default).
func startNodeInterval(ctx context.Context, t *testing.T, seeds []string, token string, interval time.Duration) (*testNode, error) {
	t.Helper()
	return launch(ctx, t, &testNode{id: uuid.NewString(), addr: freeAddr(t), dir: t.TempDir(), replInterval: interval}, seeds, token)
}

// startNodeWithVersion is startNode for a node that announces the command
// version maxVersion (ADR 0018).
func startNodeWithVersion(ctx context.Context, t *testing.T, seeds []string, token string, maxVersion uint32) (*testNode, error) {
	t.Helper()
	return launch(ctx, t, &testNode{id: uuid.NewString(), addr: freeAddr(t), dir: t.TempDir(), maxVersion: maxVersion}, seeds, token)
}

// stop stops the node n and its cluster port; its data stays on disk.
func stop(t *testing.T, n *testNode) {
	t.Helper()
	n.cancel()
	if err := n.host.Close(); err != nil {
		t.Logf("close: %v", err)
	}
	_ = n.state.Close()
}

// restart stops n and starts it again from the same data directory under a
// new address (FA-CLU-10), with the command version of n.
func restart(ctx context.Context, t *testing.T, n *testNode) *testNode {
	t.Helper()
	return restartWithVersion(ctx, t, n, n.maxVersion)
}

// restartWithVersion is restart with another command version, as after a
// binary update (maxVersion) or downgrade.
func restartWithVersion(ctx context.Context, t *testing.T, n *testNode, maxVersion uint32) *testNode {
	t.Helper()
	stop(t, n)
	moved, err := launch(ctx, t, &testNode{id: n.id, addr: freeAddr(t), dir: n.dir, replInterval: n.replInterval, caps: n.caps, maxVersion: maxVersion,
		clock: n.clock, clockInterval: n.clockInterval}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return moved
}

func launch(ctx context.Context, t *testing.T, n *testNode, seeds []string, token string) (*testNode, error) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	existing, err := cluster.HasState(n.dir)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(ctx, n.dir, n.id, "n", n.addr, "test", JoinOptions{Seeds: seeds, Token: token, MaxCommandVersion: n.maxVersion, Clock: n.clock}, log)
	if err != nil {
		return nil, err
	}
	if n.kek, err = secrets.LoadKEK(n.dir, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := prepared.CheckKEK(n.kek); err != nil {
		t.Fatal(err)
	}
	if n.state, err = sqlite.OpenState(ctx, n.dir); err != nil {
		t.Fatal(err)
	}
	if n.lastIssuedAtStart, err = n.state.LastIssuedAt(ctx); err != nil {
		t.Fatal(err)
	}
	if n.metrics, err = sqlite.OpenMetrics(ctx, n.dir, sqlite.MetricsOptions{}); err != nil {
		t.Fatal(err)
	}
	n.keyring = secrets.NewKeyring(n.kek, n.state.ListSecretKeys)
	if n.stopGrace == 0 {
		n.stopGrace = testStopGrace
	}
	if n.promotePoll == 0 {
		n.promotePoll = testPromotePoll
	}
	n.host, err = Start(ctx, Options{
		NodeID: n.id, ListenAddr: n.addr, Advertise: n.addr, DataDir: n.dir, Timeout: 300 * time.Millisecond,
		State: n.state, Metrics: n.metrics, ReplicationInterval: n.replInterval,
		KEK: n.kek, Keyring: n.keyring, Prepared: prepared, Log: log, RaftLogLevel: "error",
		OnFatal: func(err error) { t.Errorf("node halted: %v", err) },
		Name:    "n", Version: "test", Capabilities: n.caps,
		MaxCommandVersion: n.maxVersion, Clock: n.clock, ClockInterval: n.clockInterval,
		OnHeartbeatsStored:  n.onStored,
		SSRFProtection:      n.ssrfProtection,
		ProbeHealthInterval: n.probeHealthInterval,
		OnProbeRejected:     n.onProbeRejected,
		StopGrace:           n.stopGrace,
		HealthThreshold:     testHealthThreshold,
		PromotePoll:         n.promotePoll,
		RevocationReload:    testRevocationReload,
	})
	if err != nil {
		// The state is not registered for cleanup yet; close it here.
		_ = n.state.Close()
		_ = n.metrics.Close()
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	n.cancel = cancel
	go n.host.Run(runCtx)
	t.Cleanup(func() {
		cancel()
		_ = n.host.Close()
		_ = n.state.Close()
		_ = n.metrics.Close()
	})
	if !prepared.Joining && !existing {
		if err := cluster.EnsureDefaultTenant(ctx, n.host.Node, n.state); err != nil {
			t.Fatal(err)
		}
		if err := cluster.EnsureSecretKey(ctx, n.host.Node, n.state, n.kek); err != nil {
			t.Fatal(err)
		}
		if err := n.keyring.Reload(ctx); err != nil {
			t.Fatal(err)
		}
		if err := n.host.Bootstrap(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return n, nil
}

func eventually(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func isVoter(n *testNode, id string) bool {
	members, _, err := n.host.Node.Members()
	if err != nil {
		return false
	}
	for _, m := range members {
		if m.ID == id {
			return m.Voter
		}
	}
	return false
}

func memberCount(n *testNode) int {
	members, _, _ := n.host.Node.Members()
	return len(members)
}

// FA-CLU-03/04/08/10, FA-PKI-01..04: 1 → 3 voters over mTLS, removal with
// revocation.
func TestThreeVotersJoinOverMTLS(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	n1, err := startNode(ctx, t, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n1 leader", 10*time.Second, n1.host.Node.IsLeader)

	token := func(role pki.Role) string {
		tok, _, err := n1.host.Authority.CreateJoinToken(ctx, role, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	// An invalid token is rejected at once, without retries.
	bad := token(pki.RoleVoter)
	tok, _ := pki.ParseJoinToken(bad)
	forged := pki.JoinToken{ID: tok.ID, Secret: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Fingerprint: tok.Fingerprint}
	if _, err := startNode(ctx, t, []string{n1.addr}, forged.String()); !errors.Is(err, membership.ErrJoinRejected) {
		t.Fatalf("forged token: err = %v", err)
	}

	// n2 joins through the leader, n3 through the follower n2 (forwarding).
	n2, err := startNode(ctx, t, []string{n1.addr}, token(pki.RoleVoter))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n2 promoted to voter", 30*time.Second, func() bool { return isVoter(n1, n2.id) })
	n3, err := startNode(ctx, t, []string{n2.addr}, token(pki.RoleVoter))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n3 promoted to voter", 30*time.Second, func() bool { return isVoter(n1, n3.id) })

	// Every voter has the cluster's master key and the replicated state.
	for _, n := range []*testNode{n2, n3} {
		if n.kek.Fingerprint != n1.kek.Fingerprint {
			t.Errorf("node %s has another master key", n.id)
		}
	}
	cmd, _ := cluster.NewPutTenant("t-acme", "acme", cluster.TenantDocument{Name: "ACME", Plan: "default"})
	if _, err := n1.host.Node.Apply(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	for _, n := range []*testNode{n2, n3} {
		eventually(t, "tenant replicated", 10*time.Second, func() bool {
			_, err := n.state.GetTenant(ctx, "acme")
			return err == nil
		})
		eventually(t, "data keys replicated", 10*time.Second, func() bool { return n.keyring.Reload(ctx) == nil && n.keyring.ActiveKeyID() != "" })
	}

	// n2 moves to a new address; the cluster follows and n2 stays a voter.
	n2 = restart(ctx, t, n2)
	eventually(t, "n2 address updated", 30*time.Second, func() bool {
		members, _, _ := n1.host.Node.Members()
		for _, m := range members {
			if m.ID == n2.id {
				return m.Address == n2.addr && m.Voter
			}
		}
		return false
	})
	cmd2, _ := cluster.NewPutTenant("t-moved", "moved", cluster.TenantDocument{Name: "Moved", Plan: "default"})
	if _, err := n1.host.Node.Apply(ctx, cmd2); err != nil {
		t.Fatal(err)
	}
	eventually(t, "replication after the move", 15*time.Second, func() bool {
		_, err := n2.state.GetTenant(ctx, "moved")
		return err == nil
	})

	// A follower renews its certificate through the leader (FA-PKI-03).
	first := pki.SerialString(n2.host.Cert.Leaf())
	if err := n2.host.Cert.Renew(ctx, n2.host.Cert.Leaf().NotAfter, n2.host.issuer()); err != nil {
		t.Fatalf("renew via leader: %v", err)
	}
	if pki.SerialString(n2.host.Cert.Leaf()) == first {
		t.Error("follower certificate not renewed")
	}

	// A new node under n3's address is rejected with n3's ID (FA-CLU-08).
	conflict := token(pki.RoleVoter)
	_, err = membership.Join(ctx, membership.JoinOptions{
		Seeds: []string{n1.addr}, Token: conflict, NodeID: uuid.NewString(), Address: n3.addr,
		Cert: n3.host.Cert, Log: slog.New(slog.DiscardHandler),
	})
	if !errors.Is(err, membership.ErrJoinRejected) || !containsID(err, n3.id) {
		t.Errorf("address conflict: err = %v", err)
	}

	// Removal is manual and revokes the node (FA-CLU-08, FA-PKI-04).
	if _, err := n1.host.Manager.Remove(ctx, n1.id, ""); !errors.Is(err, membership.ErrRemoveLeader) {
		t.Errorf("leader removing itself: err = %v", err)
	}
	removal, err := n1.host.Manager.Remove(ctx, n3.id, "")
	if err != nil {
		t.Fatal(err)
	}
	if removal.VotersLeft != 2 || removal.Warning == "" {
		t.Errorf("removal = %+v, want a warning for 2 voters", removal)
	}
	eventually(t, "n3 out of the configuration", 10*time.Second, func() bool { return memberCount(n1) == 2 })
	if err := n1.host.Revocations.Reload(ctx, n1.state); err != nil {
		t.Fatal(err)
	}
	conn, err := clusternet.DialVoter(n1.addr, n3.host.sec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	callCtx, callCancel := context.WithTimeout(ctx, 5*time.Second)
	defer callCancel()
	if _, err := pb.NewClusterServiceClient(conn).GetStatus(callCtx, &pb.GetStatusRequest{}); err == nil {
		t.Error("removed node still reaches the cluster port")
	}
}

// FA-CLU-08: a removal asked of a follower is forwarded to the leader, which
// does the work; the leader itself and unknown nodes are rejected.
func TestRemoveForwardedByFollower(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

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
	n3, err := startNode(ctx, t, []string{n1.addr}, token())
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n3 voter", 30*time.Second, func() bool { return isVoter(n1, n3.id) })
	eventually(t, "n2 knows the leader", 10*time.Second, func() bool {
		addr, _ := n2.host.Node.Leader()
		return addr != "" && !n2.host.Node.IsLeader()
	})

	// n3 is removed through the follower n2 (forwarded to n1).
	removal, err := n2.host.Manager.Remove(ctx, n3.id, "admin-user")
	if err != nil {
		t.Fatalf("remove %s via follower: %v", n3.id, err)
	}
	if removal.VotersLeft != 2 || removal.Warning == "" {
		t.Errorf("removal = %+v, want 2 voters with a warning", removal)
	}
	eventually(t, "n3 out of the configuration", 10*time.Second, func() bool { return memberCount(n1) == 2 })
	if isVoter(n1, n3.id) {
		t.Error("n3 still a voter on the leader")
	}

	// The leader cannot be removed, even when asked through a follower.
	if _, err := n2.host.Manager.Remove(ctx, n1.id, "admin-user"); !errors.Is(err, membership.ErrRemoveLeader) {
		t.Errorf("leader removed via follower: err = %v, want ErrRemoveLeader", err)
	}
	if _, err := n2.host.Manager.Remove(ctx, "00000000-0000-4000-8000-000000000000", "admin-user"); !errors.Is(err, membership.ErrUnknownNode) {
		t.Errorf("unknown node via follower: err = %v, want ErrUnknownNode", err)
	}
	if memberCount(n1) != 2 {
		t.Errorf("rejected removals changed the configuration: %d members", memberCount(n1))
	}
}

// tenantCount counts the tenants in the state of n.
func tenantCount(t *testing.T, n *testNode) int {
	t.Helper()
	tenants, err := n.state.ListTenants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(tenants)
}

// FA-CLU-05/06, PF-04: a write proposed on a follower is forwarded to the
// leader and visible on the follower when Apply returns; a retry with the same
// request ID is deduplicated on any node, also after a leader change.
func TestWriteForwarding(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

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
	eventually(t, "n2 promoted to voter", 30*time.Second, func() bool { return isVoter(n1, n2.id) })
	n3, err := startNode(ctx, t, []string{n2.addr}, token())
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n3 promoted to voter", 30*time.Second, func() bool { return isVoter(n1, n3.id) })
	before := tenantCount(t, n1)

	const requestID = "t-fwd-1"
	doc := cluster.TenantDocument{Name: "Forwarded", Plan: "default"}

	// a) Forwarded by n2; read-after-write on n2 without waiting.
	cmd, _ := cluster.NewPutTenant(requestID, "fwd", doc)
	res, err := n2.host.Node.Apply(ctx, cmd)
	if err != nil {
		t.Fatalf("apply via follower n2: %v", err)
	}
	if res.Duplicate {
		t.Error("first application reported as duplicate")
	}
	if _, err := n2.state.GetTenant(ctx, "fwd"); err != nil {
		t.Fatalf("read after write on n2: %v", err)
	}
	if got := tenantCount(t, n2); got != before+1 {
		t.Fatalf("tenants on n2 = %d, want %d", got, before+1)
	}

	// b) The same request ID through n3 is a deduplicated retry.
	again, _ := cluster.NewPutTenant(requestID, "fwd", doc)
	res, err = n3.host.Node.Apply(ctx, again)
	if err != nil {
		t.Fatalf("retry via n3: %v", err)
	}
	if !res.Duplicate {
		t.Error("retry with the same request ID not reported as duplicate")
	}
	if got := tenantCount(t, n3); got != before+1 {
		t.Errorf("tenants on n3 after retry = %d, want %d", got, before+1)
	}

	// c) A deterministic rejection reaches the caller as *cluster.CommandError.
	invalid, _ := cluster.NewPutTenant("t-fwd-invalid", "", doc) // empty ID
	_, err = n2.host.Node.Apply(ctx, invalid)
	var cmdErr *cluster.CommandError
	if !errors.As(err, &cmdErr) || cmdErr.Code != cluster.CodeInvalid {
		t.Errorf("rejected command: err = %v, want a CommandError with code %q", err, cluster.CodeInvalid)
	}

	// d) The leader stops and hands over; the retry still deduplicates.
	n1.cancel()
	if err := n1.host.Close(); err != nil {
		t.Logf("close n1: %v", err)
	}
	eventually(t, "new leader among n2 and n3", 30*time.Second, func() bool {
		return n2.host.Node.IsLeader() || n3.host.Node.IsLeader()
	})
	retry, _ := cluster.NewPutTenant(requestID, "fwd", doc)
	res, err = n2.host.Node.Apply(ctx, retry)
	if err != nil {
		t.Fatalf("retry after leader change: %v", err)
	}
	if !res.Duplicate {
		t.Error("retry after leader change not reported as duplicate")
	}
	if got := tenantCount(t, n2); got != before+1 {
		t.Errorf("tenants after leader change = %d, want %d", got, before+1)
	}
}

func containsID(err error, id string) bool {
	return err != nil && id != "" && strings.Contains(err.Error(), id)
}

// produce stores count new heartbeats of the own stream of n and, if publish is
// set, offers them to the replication like the heartbeat writer does.
func produce(ctx context.Context, t *testing.T, n *testNode, count int, publish bool) {
	t.Helper()
	inc := n.metrics.Incarnation()
	last, err := n.metrics.LastSeq(ctx, n.id, inc)
	if err != nil {
		t.Fatal(err)
	}
	hbs := make([]store.Heartbeat, 0, count)
	for i := 1; i <= count; i++ {
		hbs = append(hbs, store.Heartbeat{
			NodeID: n.id, Incarnation: inc, Seq: last + uint64(i), TenantID: "t-acme", MonitorID: "m-web",
			TsMs: time.Now().UnixMilli(), Status: store.CheckUp, LatencyMs: 10, Details: json.RawMessage("{}"),
		})
	}
	if _, err := n.metrics.InsertHeartbeats(ctx, hbs); err != nil {
		t.Fatal(err)
	}
	if publish {
		n.host.Replication.Publish(hbs)
	}
}

// complete reports whether n holds every heartbeat of each source stream up to
// the wanted count: position, last seq and the stored rows agree.
func complete(ctx context.Context, n *testNode, want map[*testNode]uint64) bool {
	positions, err := n.metrics.StreamPositions(ctx)
	if err != nil {
		return false
	}
	pos := make(map[streamKey]uint64, len(positions))
	for _, p := range positions {
		pos[streamKey{node: p.NodeID, inc: p.Incarnation}] = p.Seq
	}
	for src, count := range want {
		key := streamKey{node: src.id, inc: src.metrics.Incarnation()}
		if pos[key] != count {
			return false
		}
		if last, err := n.metrics.LastSeq(ctx, key.node, key.inc); err != nil || last != count {
			return false
		}
		hbs, more, err := n.metrics.HeartbeatsAfter(ctx, key.node, key.inc, 0, count, 1000)
		if err != nil || more || uint64(len(hbs)) != count {
			return false
		}
	}
	return true
}

// streamKey identifies a heartbeat stream in the tests.
type streamKey struct {
	node string
	inc  string
}

// FA-REP-02/03/07: every voter ends up with the complete heartbeat streams of
// all voters. Pushed batches arrive directly; a batch that is not pushed is
// repaired by anti-entropy.
func TestHeartbeatReplication(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	const interval = 300 * time.Millisecond

	n1, err := startNodeInterval(ctx, t, nil, "", interval)
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
	n2, err := startNodeInterval(ctx, t, []string{n1.addr}, token(), interval)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n2 promoted to voter", 30*time.Second, func() bool { return isVoter(n1, n2.id) })
	n3, err := startNodeInterval(ctx, t, []string{n2.addr}, token(), interval)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n3 promoted to voter", 30*time.Second, func() bool { return isVoter(n1, n3.id) })
	nodes := []*testNode{n1, n2, n3}

	// Every voter stores and pushes 50 heartbeats of its own stream.
	want := make(map[*testNode]uint64, len(nodes))
	for _, n := range nodes {
		produce(ctx, t, n, 50, true)
		want[n] = 50
	}
	for _, n := range nodes {
		eventually(t, "pushed heartbeats on every voter", 30*time.Second, func() bool { return complete(ctx, n, want) })
	}

	// n1 stores 20 more heartbeats without pushing them, as if the batch was
	// lost on the way. Anti-entropy brings them to n2 and n3.
	produce(ctx, t, n1, 20, false)
	want[n1] = 70
	for _, n := range []*testNode{n2, n3} {
		eventually(t, "unpushed heartbeats repaired by anti-entropy", 30*time.Second, func() bool { return complete(ctx, n, want) })
	}
	if !complete(ctx, n1, want) {
		t.Error("the producing voter lost heartbeats of its own stream")
	}
}

// registry returns the node registry of n by node ID.
func registry(t *testing.T, n *testNode) map[string]store.Node {
	t.Helper()
	nodes, err := n.state.ListNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]store.Node, len(nodes))
	for _, node := range nodes {
		out[node.ID] = node
	}
	return out
}

// TestAssignment covers ADR 0017 (FA-SCH-01/02): every voter registers with its
// capabilities and is healthy; when a node stops, the leader marks it unhealthy
// and the remaining nodes agree on the new node of a count monitor.
func TestAssignment(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	n1, err := startNodeWithCaps(ctx, t, nil, "", []string{"icmp"})
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
	n2, err := startNodeWithCaps(ctx, t, []string{n1.addr}, token(), []string{"icmp"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n2 voter", 30*time.Second, func() bool { return isVoter(n1, n2.id) })
	n3, err := startNodeWithCaps(ctx, t, []string{n2.addr}, token(), nil)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n3 voter", 30*time.Second, func() bool { return isVoter(n1, n3.id) })

	// Every node has all three registered and healthy, with their capabilities.
	for _, n := range []*testNode{n1, n2, n3} {
		eventually(t, "registry complete and healthy on "+n.id, 60*time.Second, func() bool {
			reg := registry(t, n)
			for _, id := range []string{n1.id, n2.id, n3.id} {
				if r, ok := reg[id]; !ok || !r.Healthy {
					return false
				}
			}
			return true
		})
	}
	reg := registry(t, n1)
	if !reg[n1.id].HasCapability("icmp") || !reg[n2.id].HasCapability("icmp") || reg[n3.id].HasCapability("icmp") {
		t.Errorf("capabilities = %v %v %v", reg[n1.id].Capabilities, reg[n2.id].Capabilities, reg[n3.id].Capabilities)
	}

	// An ICMP monitor runs on the capable voters only (FA-CHK-31).
	nodeIDs := []string{n1.id, n2.id}
	slices.Sort(nodeIDs)
	icmpAll := scheduler.Assigner{Capability: func(string) string { return "icmp" }}
	if got := icmpAll.Nodes(store.Monitor{ID: "icmp", Type: "icmp", Assignment: store.Assignment{Mode: "all"}}, nodesOf(reg)); !slices.Equal(got, nodeIDs) {
		t.Errorf("icmp assignment = %v, want %v", got, nodeIDs)
	}

	// A count-1 monitor whose rendezvous winner is n3 while all nodes are healthy.
	all := scheduler.Assigner{}
	var monitor store.Monitor
	for range 1000 {
		m := store.Monitor{ID: uuid.NewString(), Assignment: store.Assignment{Mode: "count", Count: 1}}
		if slices.Equal(all.Nodes(m, nodesOf(reg)), []string{n3.id}) {
			monitor = m
			break
		}
	}
	if monitor.ID == "" {
		t.Fatal("no monitor with n3 as winner found")
	}

	// n3 stops; the leader marks it unhealthy within the threshold.
	n3.cancel()
	if err := n3.host.Close(); err != nil {
		t.Logf("close n3: %v", err)
	}
	eventually(t, "n3 unhealthy in the registry of the leader", 30*time.Second, func() bool {
		leader := n1
		if !n1.host.Node.IsLeader() {
			leader = n2
		}
		r, ok := registry(t, leader)[n3.id]
		return ok && !r.Healthy
	})
	eventually(t, "n3 unhealthy on both survivors", 15*time.Second, func() bool {
		r1, ok1 := registry(t, n1)[n3.id]
		r2, ok2 := registry(t, n2)[n3.id]
		return ok1 && ok2 && !r1.Healthy && !r2.Healthy
	})

	// Both survivors compute the same new node, and it is not n3.
	a1 := all.Nodes(monitor, nodesOf(registry(t, n1)))
	a2 := all.Nodes(monitor, nodesOf(registry(t, n2)))
	if !slices.Equal(a1, a2) {
		t.Errorf("assignments differ: n1 %v, n2 %v", a1, a2)
	}
	if len(a1) != 1 || (a1[0] != n1.id && a1[0] != n2.id) {
		t.Errorf("assignment = %v, want one of n1 or n2", a1)
	}
}

// nodesOf returns the registry entries of reg as a slice.
func nodesOf(reg map[string]store.Node) []store.Node {
	out := make([]store.Node, 0, len(reg))
	for _, n := range reg {
		out = append(out, n)
	}
	return out
}

// TestRollingUpdate covers ADR 0018 (FA-CLU-12, PF-09): a voter with an older
// binary holds the cluster command level back; the level rises once it is
// updated; a downgrade does not start; a join with an old binary is rejected.
func TestRollingUpdate(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// The node with version 2 bootstraps the cluster. A single voter sets the
	// level, so the old node must be in the configuration before the others
	// join; otherwise the level would already be 3 when it joins.
	c, err := startNodeWithVersion(ctx, t, nil, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "c leader", 10*time.Second, c.host.Node.IsLeader)
	token := func() string {
		tok, _, err := c.host.Authority.CreateJoinToken(ctx, pki.RoleVoter, "", 0)
		if err != nil {
			t.Fatal(err, c.dir, c.id, c.addr)
		}
		return tok
	}
	a, err := startNodeWithVersion(ctx, t, []string{c.addr}, token(), 3)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a voter", 30*time.Second, func() bool { return isVoter(c, a.id) })
	b, err := startNodeWithVersion(ctx, t, []string{a.addr}, token(), 3)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b voter", 30*time.Second, func() bool { return isVoter(c, b.id) })

	// All three are registered with their versions.
	for _, n := range []*testNode{c, a, b} {
		eventually(t, "registry with versions on "+n.id, 60*time.Second, func() bool {
			reg := registry(t, n)
			return reg[c.id].MaxCommandVersion == 2 && reg[a.id].MaxCommandVersion == 3 && reg[b.id].MaxCommandVersion == 3
		})
	}

	// The level stays at 2 while c is a voter (ADR 0018, option A).
	requireLevel(ctx, t, []*testNode{c, a, b}, 2, 3*time.Second)

	// A command of version 3 is rejected while c supports only 2.
	early := cluster.NewSetNodeHealth(uuid.NewString(), a.id, true)
	early.Version = 3
	_, err = a.host.Node.Apply(ctx, early)
	var cmdErr *cluster.CommandError
	if !errors.As(err, &cmdErr) || cmdErr.Code != cluster.CodeNotEnabled {
		t.Fatalf("version 3 command at level 2: err = %v, want CommandError %q", err, cluster.CodeNotEnabled)
	}

	// c is updated: it restarts with version 3 and the level rises to 3.
	c = restartWithVersion(ctx, t, c, 3)
	nodes := []*testNode{c, a, b}
	for _, n := range nodes {
		eventually(t, "command level 3 on "+n.id, 60*time.Second, func() bool {
			level, err := n.host.Node.CommandLevel(ctx)
			return err == nil && level == 3
		})
	}

	// A version 3 command now succeeds through a follower and is applied on
	// every voter, without the FSM halting.
	follower := a
	if a.host.Node.IsLeader() {
		follower = b
	}
	ok := cluster.NewSetNodeHealth(uuid.NewString(), a.id, true)
	ok.Version = 3
	res, err := follower.host.Node.Apply(ctx, ok)
	if err != nil {
		t.Fatalf("version 3 command at level 3 via %s: %v", follower.id, err)
	}
	for _, n := range nodes {
		eventually(t, "version 3 command applied on "+n.id, 15*time.Second, func() bool {
			return n.host.Node.Status().AppliedIndex >= res.Index
		})
		if err := n.host.Node.FSM().Healthy(); err != nil {
			t.Errorf("FSM of %s halted: %v", n.id, err)
		}
	}

	// A downgrade does not start: the state needs version 3.
	stop(t, c)
	downgraded := &testNode{id: c.id, addr: freeAddr(t), dir: c.dir, maxVersion: 2}
	if _, err := launch(ctx, t, downgraded, nil, ""); !errors.Is(err, cluster.ErrIncompatibleVersion) {
		t.Fatalf("downgrade start: err = %v, want ErrIncompatibleVersion", err)
	}

	// A new node with version 2 is refused as a voter, and the token is
	// kept. The rejection is permanent, so the join ends before the context.
	joinCtx, joinCancel := context.WithTimeout(ctx, 30*time.Second)
	defer joinCancel()
	oldToken, _, err := a.host.Authority.CreateJoinToken(joinCtx, pki.RoleVoter, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	old := &testNode{id: uuid.NewString(), addr: freeAddr(t), dir: t.TempDir(), maxVersion: 2}
	_, err = launch(joinCtx, t, old, []string{a.addr}, oldToken)
	if !errors.Is(err, membership.ErrJoinRejected) || !strings.Contains(err.Error(), "upgrade the binary") {
		t.Fatalf("join with version 2: err = %v, want ErrJoinRejected mentioning the upgrade", err)
	}
	requireLevel(ctx, t, []*testNode{a, b}, 3, time.Second)
}

// requireLevel checks for d that every node reports the command level want.
func requireLevel(ctx context.Context, t *testing.T, nodes []*testNode, want uint32, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		for _, n := range nodes {
			level, err := n.host.Node.CommandLevel(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if level != want {
				t.Fatalf("command level on %s = %d, want %d", n.id, level, want)
			}
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
